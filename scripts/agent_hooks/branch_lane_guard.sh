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
  if git grep -n -I -i -E "$internal_dev_pattern" "$REF" -- \
      "${distribution_text_paths[@]}" "${distribution_text_excludes[@]}" \
      >/tmp/contextlattice_internal_dev_doc_hits.txt 2>/dev/null; then
    cat /tmp/contextlattice_internal_dev_doc_hits.txt >&2
    blocked=1
  fi

  private_reference_pattern='docs[/\\]+private[/\\]+|private_docs[/\\]+|(^|[^[:alnum:]_.-])(\.\.?[/\\]+)+private[/\\]+'
  if git grep -n -I -i -E "$private_reference_pattern" "$REF" -- \
      "${distribution_text_paths[@]}" "${distribution_text_excludes[@]}" \
      >/tmp/contextlattice_private_reference_hits.txt 2>/dev/null; then
    cat /tmp/contextlattice_private_reference_hits.txt >&2
    blocked=1
  fi

  # shellcheck disable=SC2016 # The regex intentionally matches literal shell-home prefixes.
  private_repo_pattern='sheawinkler/'"http-context-and-memory-orchestrator"
  operator_checkout_pattern='/(Users|home)/[[:alnum:]_.-]+/|[[:alpha:]]:[/\\]+Users[/\\]+[[:alnum:]_.-]+[/\\]+|(\$HOME|~)[/\\]+Documents[/\\]+Projects[/\\]+|context-lattice-private|crypto_trader_post_training_needs_godmode_and_finalization|'"${private_repo_pattern}"
  if git grep -n -I -i -E "$operator_checkout_pattern" "$REF" -- \
      "${distribution_text_paths[@]}" "${distribution_text_excludes[@]}" \
      >/tmp/contextlattice_operator_checkout_hits.txt 2>/dev/null; then
    cat /tmp/contextlattice_operator_checkout_hits.txt >&2
    blocked=1
  fi

  # The helper itself is blocklisted, but a leaked source/call from a shared
  # launcher would still break a distribution tree where that helper is absent.
  if git grep -n -I -E 'private_dev_posture' "$REF" -- \
      launch.sh scripts/compose_v4_balanced.sh \
      >/tmp/contextlattice_private_dev_launcher_hits.txt 2>/dev/null; then
    cat /tmp/contextlattice_private_dev_launcher_hits.txt >&2
    blocked=1
  fi
fi

if [[ "$LANE" == "public" ]]; then
  internal_doc_reference='docs/private/|private_docs/'
  if git grep -n -I -E "$internal_doc_reference" "$REF" -- \
      README.md docs/public_overview docs/releases launch_service packaging \
      >/tmp/contextlattice_internal_doc_reference_hits.txt 2>/dev/null; then
    cat /tmp/contextlattice_internal_doc_reference_hits.txt >&2
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
  if git grep -n -I -E "$paid_gateway_pattern" "$REF" -- services/gateway-go >/tmp/contextlattice_paid_gateway_hits.txt 2>/dev/null; then
    cat /tmp/contextlattice_paid_gateway_hits.txt >&2
    blocked=1
  fi

  paid_ui_pattern='(/api/billing/(download|downloads|download-token|entitlement|summary|stripe|paypal|solana-pay|kraken)|/api/support/diagnostics|/api/telemetry/pro-analytics|/api/workspace/(members|invitations)|exportSupportDiagnostics|HostedArtifacts|WorkspaceInvitation|workspaceInvitations|activeWorkspaceId|Workspace people)'
  if git grep -n -I -E "$paid_ui_pattern" "$REF" -- \
      contextlattice-dashboard/app \
      contextlattice-dashboard/components \
      contextlattice-dashboard/lib \
      contextlattice-dashboard/prisma \
      contextlattice-dashboard/tests \
      >/tmp/contextlattice_paid_ui_hits.txt 2>/dev/null; then
    cat /tmp/contextlattice_paid_ui_hits.txt >&2
    blocked=1
  fi

  paid_distribution_pattern='Set-PaidRuntimePosture|apply_paid_runtime_posture|set_env_value.*GO_V4_|Set-EnvValue.*GO_V4_|EXPECTED_SOURCE_REF.*public-paid/main|SOURCE_TRACKING_REF.*public-paid/main'
  if git grep -n -I -E "$paid_distribution_pattern" "$REF" -- \
      packaging scripts/build_release_payload.sh scripts/build_linux_bundle.sh \
      scripts/build_macos_dmg.sh scripts/build_windows_msi.sh \
      >/tmp/contextlattice_paid_distribution_hits.txt 2>/dev/null; then
    cat /tmp/contextlattice_paid_distribution_hits.txt >&2
    blocked=1
  fi
fi

if [[ "$LANE" == "public" || "$LANE" == "public-paid" ]]; then
  machine_pattern="${CONTEXTLATTICE_PUBLIC_FORBIDDEN_PATH_RE:-}"
  if [[ -n "$machine_pattern" ]]; then
    if [[ "$REF" == "HEAD" ]]; then
      if rg -n --hidden --glob '!.git/**' --glob '!node_modules/**' --glob '!tmp/**' --glob '!archive/**' --glob '!private_docs/**' --glob '!docs/private/**' "$machine_pattern" . >/tmp/contextlattice_lane_hits.txt 2>/dev/null; then
        cat /tmp/contextlattice_lane_hits.txt >&2
        blocked=1
      fi
    else
      if git grep -n -I -E "$machine_pattern" "$REF" -- . ':(exclude)docs/private/**' ':(exclude)private_docs/**' ':(exclude)node_modules/**' ':(exclude)tmp/**' >/tmp/contextlattice_lane_hits.txt 2>/dev/null; then
        cat /tmp/contextlattice_lane_hits.txt >&2
        blocked=1
      fi
    fi
  fi
fi

[[ "$blocked" == "0" ]] || fail "lane hygiene failed for ${LANE}"
emit_json_kv ok=true lane="$LANE" branch="$branch" ref="$REF"
