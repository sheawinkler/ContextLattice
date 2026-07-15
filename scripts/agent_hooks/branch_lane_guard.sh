#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: branch_lane_guard.sh [--lane auto|private|public|public-paid] [--ref <git-ref>]

Enforces existing lane-specific repository hygiene.

Lane model:
  private     origin/main; everything allowed.
  public      public/main; OSS-safe only.
  public-paid public-paid/main; premium paid lane, including paid docs.
USAGE
}

LANE="auto"
REF="HEAD"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --lane) LANE="$2"; shift 2 ;;
    --ref) REF="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

ROOT="$(repo_root)"
cd "$ROOT"
branch="$(git branch --show-current)"
case "$REF" in
  origin/public/main|refs/remotes/origin/public/main)
    fail "deprecated public production alias: use refs/remotes/public/main"
    ;;
  origin/public-paid/main|refs/remotes/origin/public-paid/main)
    fail "deprecated public-paid production alias: use refs/remotes/public-paid/main"
    ;;
esac
git rev-parse --verify "$REF" >/dev/null 2>&1 || fail "missing ref: $REF"
if [[ "$LANE" == "auto" ]]; then
  lane_source="$branch"
  [[ "$REF" != "HEAD" ]] && lane_source="$REF"
  case "$lane_source" in
    */public/main|public/main|public-main|main-public) LANE="public" ;;
    */public-paid/main|public-paid/main|public-paid-main|public-paid*) LANE="public-paid" ;;
    *) LANE="private" ;;
  esac
fi

blocked=0
scan_tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-branch-lane.XXXXXX")" || \
  fail "could not create lane scan workspace"
cleanup_lane_scan() {
  rm -rf "$scan_tmp_dir"
}
trap cleanup_lane_scan EXIT

scan_to_file() {
  local output="$1"
  local label="$2"
  local errors="${output}.stderr"
  local status
  shift 2

  if ! exec 3>"$output"; then
    fail "${label} could not open scan output"
  fi
  if ! exec 4>"$errors"; then
    exec 3>&-
    fail "${label} could not open scan diagnostics"
  fi
  if "$@" >&3 2>&4; then
    status=0
  else
    status=$?
  fi
  exec 3>&- || fail "${label} could not close scan output"
  exec 4>&- || fail "${label} could not close scan diagnostics"
  case "$status" in
    0)
      if [[ -s "$errors" ]]; then
        cat "$errors" >&2
        fail "${label} produced unexpected stderr"
      fi
      return 0
      ;;
    1)
      if [[ -s "$output" || -s "$errors" ]]; then
        cat "$errors" >&2
        fail "${label} returned contradictory no-match output"
      fi
      return 1
      ;;
    *)
      cat "$errors" >&2
      fail "${label} failed with status ${status}"
      ;;
  esac
}

# Distribution namespaces are portable ASCII and case-insensitive by contract.
shopt -s nocasematch
if [[ "$LANE" == "public" ]]; then
  while IFS= read -r path; do
    case "$path" in
      docs/private/*|private_docs/*|private/*|.ops/*)
        printf '[branch_lane_guard] BLOCK private path in %s lane: %s\n' "$LANE" "$path" >&2
        blocked=1
        ;;
    esac
  done < <(git ls-tree -r --name-only "$REF")
fi

if [[ "$LANE" == "public-paid" ]]; then
  while IFS= read -r path; do
    case "$path" in
      docs/private/*|private_docs/*|private/*|*.private.md|\
      .github/workflows/capability-parity.yml|\
      config/env/premium_dev.env|\
      scripts/agent/audit-frontier-30-program|\
      scripts/agent/audit-private-paid-superset|\
      scripts/launch_private_dev.sh|\
      scripts/lib/private_dev_posture.sh|\
      scripts/setup_paid_local_env.sh|\
      scripts/tests/test_private_dev_posture.py)
        printf '[branch_lane_guard] BLOCK private-only path in %s lane: %s\n' "$LANE" "$path" >&2
        blocked=1
        ;;
      .backup/*|dev/backups/*|development/*|logs/*|\
      *.pid|*.bak|*.bak.*|*.tmp|\
      .env|*/.env|.env_*|*/.env_*|\
      .ops/snapshots/*)
        printf '[branch_lane_guard] BLOCK tracked scratch/backup path in %s lane: %s\n' "$LANE" "$path" >&2
        blocked=1
        ;;
    esac
  done < <(git ls-tree -r --name-only "$REF")
fi

if [[ "$LANE" == "public" || "$LANE" == "public-paid" ]]; then
  distribution_text_paths=(
    AGENTS.md
    CODE_OF_CONDUCT.md
    CONTRIBUTING.md
    README.md
    SECURITY.md
    docs
    launch_service
    packaging
    justfile
    .env.example
    scripts/devnet_smoke.sh
  )
  distribution_text_excludes=(
    ':(exclude)docs/private/**'
    ':(exclude)private_docs/**'
  )

  internal_dev_pattern='private[- ]development.*(keyless|superset|bypass|internal-only|private-only)|(^|[^[:alnum:]_])private-dev([^[:alnum:]_]|$)|keyless (superset|bypass)|unlocked superset'
  internal_dev_hits="${scan_tmp_dir}/internal-dev-doc-hits"
  if scan_to_file "$internal_dev_hits" "internal development reference scan" \
      git grep -n -I -i -E "$internal_dev_pattern" "$REF" -- \
      "${distribution_text_paths[@]}" "${distribution_text_excludes[@]}"; then
    cat "$internal_dev_hits" >&2
    blocked=1
  fi

  private_reference_pattern='docs[/\\]+private[/\\]+|private_docs[/\\]+|(^|[^[:alnum:]_.-])(\.\.?[/\\]+)+private[/\\]+'
  private_reference_hits="${scan_tmp_dir}/private-reference-hits"
  if scan_to_file "$private_reference_hits" "private documentation reference scan" \
      git grep -n -I -i -E "$private_reference_pattern" "$REF" -- \
      "${distribution_text_paths[@]}" "${distribution_text_excludes[@]}"; then
    cat "$private_reference_hits" >&2
    blocked=1
  fi

  # shellcheck disable=SC2016 # The regex intentionally matches literal shell-home prefixes.
  private_repo_pattern='sheawinkler/'"http-context-and-memory-orchestrator"
  # shellcheck disable=SC2016 # The regex intentionally matches literal shell-home prefixes.
  operator_checkout_pattern='/(Users|home)/[[:alnum:]_.-]+/|[[:alpha:]]:[/\\]+Users[/\\]+[[:alnum:]_.-]+[/\\]+|(\$HOME|~)[/\\]+Documents[/\\]+Projects[/\\]+|context-lattice-private|crypto_trader_post_training_needs_godmode_and_finalization|'"${private_repo_pattern}"
  operator_checkout_hits="${scan_tmp_dir}/operator-checkout-hits"
  if scan_to_file "$operator_checkout_hits" "operator checkout reference scan" \
      git grep -n -I -i -E "$operator_checkout_pattern" "$REF" -- \
      "${distribution_text_paths[@]}" "${distribution_text_excludes[@]}"; then
    cat "$operator_checkout_hits" >&2
    blocked=1
  fi

  # The helper itself is blocklisted, but a leaked source/call from a shared
  # launcher would still break a distribution tree where that helper is absent.
  private_dev_launcher_hits="${scan_tmp_dir}/private-dev-launcher-hits"
  if scan_to_file "$private_dev_launcher_hits" "private development launcher scan" \
      git grep -n -I -E 'private_dev_posture' "$REF" -- \
      launch.sh scripts/compose_v4_balanced.sh; then
    cat "$private_dev_launcher_hits" >&2
    blocked=1
  fi
fi

if [[ "$LANE" == "public" ]]; then
  internal_doc_reference='docs/private/|private_docs/'
  internal_doc_reference_hits="${scan_tmp_dir}/internal-doc-reference-hits"
  if scan_to_file "$internal_doc_reference_hits" "restricted documentation link scan" \
      git grep -n -I -E "$internal_doc_reference" "$REF" -- \
      README.md docs/public_overview docs/releases launch_service packaging; then
    cat "$internal_doc_reference_hits" >&2
    blocked=1
  fi
fi

if [[ "$LANE" == "public" ]]; then
  blocklist="${CONTEXTLATTICE_PUBLIC_SYNC_BLOCKLIST:-${ROOT}/config/public_sync_blocklist.txt}"
  if [[ -f "$blocklist" ]]; then
    while IFS= read -r pattern; do
      pattern="${pattern%%#*}"
      pattern="$(printf '%s' "$pattern" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
      [[ -n "$pattern" ]] || continue
      while IFS= read -r path; do
        # shellcheck disable=SC2254 # Blocklist entries are intentional globs.
        case "$path" in
          $pattern)
            printf '[branch_lane_guard] BLOCK public-sync pattern %s: %s\n' "$pattern" "$path" >&2
            blocked=1
            ;;
        esac
      done < <(git ls-tree -r --name-only "$REF")
    done <"$blocklist"
  fi

  paid_gateway_pattern='GO_(PAID|V4)_ENTITLEMENT|enforce(Paid|V4)Entitlement|runtimeLicenseVerifier|runtimeLicenseSchemaID'
  paid_gateway_hits="${scan_tmp_dir}/paid-gateway-hits"
  if scan_to_file "$paid_gateway_hits" "paid gateway surface scan" \
      git grep -n -I -E "$paid_gateway_pattern" "$REF" -- services/gateway-go; then
    cat "$paid_gateway_hits" >&2
    blocked=1
  fi

  paid_ui_pattern='(/api/billing/(download|downloads|download-token|entitlement|summary|stripe|paypal|solana-pay|kraken)|/api/support/diagnostics|/api/telemetry/pro-analytics|/api/workspace/(members|invitations)|exportSupportDiagnostics|HostedArtifacts|WorkspaceInvitation|workspaceInvitations|activeWorkspaceId|Workspace people)'
  paid_ui_hits="${scan_tmp_dir}/paid-ui-hits"
  if scan_to_file "$paid_ui_hits" "paid dashboard surface scan" \
      git grep -n -I -E "$paid_ui_pattern" "$REF" -- \
      contextlattice-dashboard/app \
      contextlattice-dashboard/components \
      contextlattice-dashboard/lib \
      contextlattice-dashboard/prisma \
      contextlattice-dashboard/tests; then
    cat "$paid_ui_hits" >&2
    blocked=1
  fi

  paid_distribution_pattern='Set-PaidRuntimePosture|apply_paid_runtime_posture|set_env_value.*GO_V4_|Set-EnvValue.*GO_V4_|EXPECTED_SOURCE_REF.*public-paid/main|SOURCE_TRACKING_REF.*public-paid/main'
  paid_distribution_hits="${scan_tmp_dir}/paid-distribution-hits"
  if scan_to_file "$paid_distribution_hits" "paid distribution surface scan" \
      git grep -n -I -E "$paid_distribution_pattern" "$REF" -- \
      packaging scripts/build_release_payload.sh scripts/build_linux_bundle.sh \
      scripts/build_macos_dmg.sh scripts/build_windows_msi.sh; then
    cat "$paid_distribution_hits" >&2
    blocked=1
  fi
fi

if [[ "$LANE" == "public" || "$LANE" == "public-paid" ]]; then
  machine_pattern="${CONTEXTLATTICE_PUBLIC_FORBIDDEN_PATH_RE:-}"
  if [[ -n "$machine_pattern" ]]; then
    machine_hits="${scan_tmp_dir}/machine-path-hits"
    if [[ "$REF" == "HEAD" ]]; then
      if scan_to_file "$machine_hits" "working-tree forbidden path scan" \
          rg -n --hidden --glob '!.git/**' --glob '!node_modules/**' --glob '!tmp/**' --glob '!archive/**' --glob '!private_docs/**' --glob '!docs/private/**' "$machine_pattern" .; then
        cat "$machine_hits" >&2
        blocked=1
      fi
    else
      if scan_to_file "$machine_hits" "committed forbidden path scan" \
          git grep -n -I -E "$machine_pattern" "$REF" -- . ':(exclude)docs/private/**' ':(exclude)private_docs/**' ':(exclude)node_modules/**' ':(exclude)tmp/**'; then
        cat "$machine_hits" >&2
        blocked=1
      fi
    fi
  fi
fi

[[ "$blocked" == "0" ]] || fail "lane hygiene failed for ${LANE}"
emit_json_kv ok=true lane="$LANE" branch="$branch" ref="$REF"
