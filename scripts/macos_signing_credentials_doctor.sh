#!/usr/bin/env bash
set -euo pipefail

REPOS="${CONTEXTLATTICE_MACOS_SECRET_REPOS:-sheawinkler/http-context-and-memory-orchestrator,sheawinkler/ContextLattice}"
PROFILE="${CONTEXTLATTICE_MACOS_NOTARY_KEYCHAIN_PROFILE:-}"
LIVE=0

usage() {
  cat <<'USAGE'
Usage: scripts/macos_signing_credentials_doctor.sh [options]

Checks local macOS signing/notary state and GitHub secret/variable presence
without printing secret values.

Options:
  --repos <a,b>      GitHub repos to inspect
  --profile <name>   Optional local notarytool keychain profile to validate
  --live             Run a live notarytool history probe for --profile
  -h, --help         Show this help
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repos)
      [[ $# -ge 2 ]] || { echo "Missing value for --repos" >&2; exit 2; }
      REPOS="$2"
      shift 2
      ;;
    --profile)
      [[ $# -ge 2 ]] || { echo "Missing value for --profile" >&2; exit 2; }
      PROFILE="$2"
      shift 2
      ;;
    --live)
      LIVE=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

required_secret_names=(
  CONTEXTLATTICE_MACOS_CERT_P12_BASE64
  CONTEXTLATTICE_MACOS_CERT_P12_PASSWORD
  CONTEXTLATTICE_MACOS_KEYCHAIN_PASSWORD
  CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY
)

notary_secret_groups=(
  "api_key:CONTEXTLATTICE_MACOS_NOTARY_KEY_P8_BASE64,CONTEXTLATTICE_MACOS_NOTARY_KEY_ID"
  "apple_id:CONTEXTLATTICE_MACOS_NOTARY_APPLE_ID,CONTEXTLATTICE_MACOS_NOTARY_TEAM_ID,CONTEXTLATTICE_MACOS_NOTARY_PASSWORD"
  "keychain_profile:CONTEXTLATTICE_MACOS_NOTARY_KEYCHAIN_PROFILE"
)

required_vars=(
  CONTEXTLATTICE_MACOS_SIGNING_REQUIRED
  CONTEXTLATTICE_MACOS_NOTARIZATION_REQUIRED
)

has_name() {
  local needle="$1"
  local haystack="$2"
  grep -Fxq "$needle" <<<"${haystack}"
}

echo "local.codesigning:"
if [[ "$(uname -s)" == "Darwin" ]]; then
  identities="$(security find-identity -v -p codesigning 2>&1 || true)"
  printf '%s\n' "${identities}" | sed -n '1,80p'
  developer_id_count="$(grep -c 'Developer ID Application' <<<"${identities}" || true)"
  echo "local.developer_id_application_count=${developer_id_count}"
else
  echo "local.codesigning_skipped=non_darwin"
fi

if [[ -n "${PROFILE}" ]]; then
  echo "local.notary_profile=${PROFILE}"
  if [[ "${LIVE}" == "1" ]]; then
    xcrun notarytool history --keychain-profile "${PROFILE}" --output-format json >/dev/null
    echo "local.notary_profile_live=ok"
  else
    echo "local.notary_profile_live=skipped"
  fi
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "github=skipped_missing_gh"
  exit 0
fi

IFS=',' read -r -a repo_list <<<"${REPOS}"
for repo in "${repo_list[@]}"; do
  repo="$(printf '%s' "${repo}" | xargs)"
  [[ -n "${repo}" ]] || continue
  echo "github.repo=${repo}"
  secrets="$(gh secret list --repo "${repo}" --json name --jq '.[].name' 2>/dev/null || true)"
  vars="$(gh variable list --repo "${repo}" --json name --jq '.[].name' 2>/dev/null || true)"

  for name in "${required_secret_names[@]}"; do
    if has_name "${name}" "${secrets}"; then
      echo "  secret.${name}=present"
    else
      echo "  secret.${name}=missing"
    fi
  done

  for group in "${notary_secret_groups[@]}"; do
    label="${group%%:*}"
    csv="${group#*:}"
    IFS=',' read -r -a names <<<"${csv}"
    present=0
    for name in "${names[@]}"; do
      if has_name "${name}" "${secrets}"; then
        present=$((present + 1))
      fi
    done
    echo "  notary_group.${label}=${present}/${#names[@]}"
  done

  for name in "${required_vars[@]}"; do
    if has_name "${name}" "${vars}"; then
      echo "  variable.${name}=present"
    else
      echo "  variable.${name}=missing"
    fi
  done
done
