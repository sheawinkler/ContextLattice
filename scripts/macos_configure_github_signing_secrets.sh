#!/usr/bin/env bash
set -euo pipefail

REPOS="${CONTEXTLATTICE_MACOS_SECRET_REPOS:-sheawinkler/http-context-and-memory-orchestrator,sheawinkler/ContextLattice}"
CERT_P12_PATH="${CONTEXTLATTICE_MACOS_CERT_P12_PATH:-}"
CERT_PASSWORD="${CONTEXTLATTICE_MACOS_CERT_P12_PASSWORD:-}"
KEYCHAIN_PASSWORD="${CONTEXTLATTICE_MACOS_KEYCHAIN_PASSWORD:-}"
CODESIGN_IDENTITY="${CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY:-}"
NOTARY_KEY_P8_PATH="${CONTEXTLATTICE_MACOS_NOTARY_KEY_P8_PATH:-}"
NOTARY_KEY_ID="${CONTEXTLATTICE_MACOS_NOTARY_KEY_ID:-}"
NOTARY_ISSUER_ID="${CONTEXTLATTICE_MACOS_NOTARY_ISSUER_ID:-}"
NOTARY_APPLE_ID="${CONTEXTLATTICE_MACOS_NOTARY_APPLE_ID:-}"
NOTARY_TEAM_ID="${CONTEXTLATTICE_MACOS_NOTARY_TEAM_ID:-}"
NOTARY_PASSWORD="${CONTEXTLATTICE_MACOS_NOTARY_PASSWORD:-}"
REQUIRED_GATES=0
DRY_RUN=0

usage() {
  cat <<'USAGE'
Usage: scripts/macos_configure_github_signing_secrets.sh [options]

Configures GitHub secrets for ContextLattice macOS Developer ID signing and
notarization. Secret values are read from files, environment variables, or
secure prompts; they are not printed.

Options:
  --repos <a,b>              GitHub repos to configure
  --cert-p12 <path>          Exported Developer ID Application .p12 file
  --codesign-identity <id>   Developer ID Application identity string
  --notary-key-p8 <path>     App Store Connect API key .p8 file
  --notary-key-id <id>       App Store Connect API key id
  --notary-issuer-id <uuid>  App Store Connect issuer id, omit for Individual API keys
  --apple-id <email>         Apple ID notarization fallback
  --team-id <id>             Apple Developer Team ID
  --required-gates           Set signing/notarization required vars to true
  --dry-run                  Show what would be configured
  -h, --help                 Show this help

Environment alternatives:
  CONTEXTLATTICE_MACOS_CERT_P12_PASSWORD
  CONTEXTLATTICE_MACOS_KEYCHAIN_PASSWORD
  CONTEXTLATTICE_MACOS_NOTARY_PASSWORD
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repos)
      [[ $# -ge 2 ]] || { echo "Missing value for --repos" >&2; exit 2; }
      REPOS="$2"
      shift 2
      ;;
    --cert-p12)
      [[ $# -ge 2 ]] || { echo "Missing value for --cert-p12" >&2; exit 2; }
      CERT_P12_PATH="$2"
      shift 2
      ;;
    --codesign-identity)
      [[ $# -ge 2 ]] || { echo "Missing value for --codesign-identity" >&2; exit 2; }
      CODESIGN_IDENTITY="$2"
      shift 2
      ;;
    --notary-key-p8)
      [[ $# -ge 2 ]] || { echo "Missing value for --notary-key-p8" >&2; exit 2; }
      NOTARY_KEY_P8_PATH="$2"
      shift 2
      ;;
    --notary-key-id)
      [[ $# -ge 2 ]] || { echo "Missing value for --notary-key-id" >&2; exit 2; }
      NOTARY_KEY_ID="$2"
      shift 2
      ;;
    --notary-issuer-id)
      [[ $# -ge 2 ]] || { echo "Missing value for --notary-issuer-id" >&2; exit 2; }
      NOTARY_ISSUER_ID="$2"
      shift 2
      ;;
    --apple-id)
      [[ $# -ge 2 ]] || { echo "Missing value for --apple-id" >&2; exit 2; }
      NOTARY_APPLE_ID="$2"
      shift 2
      ;;
    --team-id)
      [[ $# -ge 2 ]] || { echo "Missing value for --team-id" >&2; exit 2; }
      NOTARY_TEAM_ID="$2"
      shift 2
      ;;
    --required-gates)
      REQUIRED_GATES=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
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

require_file() {
  local path="$1"
  local label="$2"
  if [[ -z "${path}" || ! -f "${path}" ]]; then
    echo "ERROR: ${label} file not found: ${path:-<missing>}" >&2
    exit 1
  fi
}

prompt_secret() {
  local var_name="$1"
  local prompt="$2"
  local current="${!var_name:-}"
  if [[ -n "${current}" ]]; then
    return 0
  fi
  if [[ ! -t 0 ]]; then
    echo "ERROR: ${var_name} is required in non-interactive mode." >&2
    exit 1
  fi
  local value
  read -r -s -p "${prompt}: " value
  echo
  printf -v "${var_name}" '%s' "${value}"
}

auto_identity() {
  if [[ -n "${CODESIGN_IDENTITY}" || "$(uname -s)" != "Darwin" ]]; then
    return 0
  fi
  CODESIGN_IDENTITY="$(
    security find-identity -v -p codesigning 2>/dev/null |
      sed -n 's/.*"\(Developer ID Application: .*\)"/\1/p' |
      head -1
  )"
}

set_secret() {
  local repo="$1"
  local name="$2"
  local value="$3"
  if [[ "${DRY_RUN}" == "1" ]]; then
    echo "  would_set_secret.${name}=yes"
    return 0
  fi
  printf '%s' "${value}" | gh secret set "${name}" --repo "${repo}" --body-file - >/dev/null
  echo "  secret.${name}=set"
}

set_variable() {
  local repo="$1"
  local name="$2"
  local value="$3"
  if [[ "${DRY_RUN}" == "1" ]]; then
    echo "  would_set_variable.${name}=${value}"
    return 0
  fi
  gh variable set "${name}" --repo "${repo}" --body "${value}" >/dev/null
  echo "  variable.${name}=set"
}

if ! command -v gh >/dev/null 2>&1; then
  echo "ERROR: gh CLI is required." >&2
  exit 1
fi

require_file "${CERT_P12_PATH}" "Developer ID .p12"
auto_identity
if [[ -z "${CODESIGN_IDENTITY}" ]]; then
  echo "ERROR: no codesign identity provided and no local Developer ID Application identity was found." >&2
  exit 1
fi

prompt_secret CERT_PASSWORD "Developer ID .p12 password"
prompt_secret KEYCHAIN_PASSWORD "CI temporary keychain password"

notary_mode=""
if [[ -n "${NOTARY_KEY_P8_PATH}" || -n "${NOTARY_KEY_ID}" || -n "${NOTARY_ISSUER_ID}" ]]; then
  require_file "${NOTARY_KEY_P8_PATH}" "App Store Connect .p8"
  if [[ -z "${NOTARY_KEY_ID}" ]]; then
    echo "ERROR: --notary-key-id is required with --notary-key-p8." >&2
    exit 1
  fi
  notary_mode="api_key"
elif [[ -n "${NOTARY_APPLE_ID}" || -n "${NOTARY_TEAM_ID}" ]]; then
  if [[ -z "${NOTARY_APPLE_ID}" || -z "${NOTARY_TEAM_ID}" ]]; then
    echo "ERROR: --apple-id and --team-id are both required for Apple ID notarization." >&2
    exit 1
  fi
  prompt_secret NOTARY_PASSWORD "Apple ID app-specific password"
  notary_mode="apple_id"
else
  echo "ERROR: configure either App Store Connect API key notarization or Apple ID app-specific password notarization." >&2
  exit 1
fi

cert_b64="$(base64 <"${CERT_P12_PATH}" | tr -d '\n')"
if [[ "${notary_mode}" == "api_key" ]]; then
  notary_key_b64="$(base64 <"${NOTARY_KEY_P8_PATH}" | tr -d '\n')"
fi

IFS=',' read -r -a repo_list <<<"${REPOS}"
for repo in "${repo_list[@]}"; do
  repo="$(printf '%s' "${repo}" | xargs)"
  [[ -n "${repo}" ]] || continue
  echo "github.repo=${repo}"
  set_secret "${repo}" CONTEXTLATTICE_MACOS_CERT_P12_BASE64 "${cert_b64}"
  set_secret "${repo}" CONTEXTLATTICE_MACOS_CERT_P12_PASSWORD "${CERT_PASSWORD}"
  set_secret "${repo}" CONTEXTLATTICE_MACOS_KEYCHAIN_PASSWORD "${KEYCHAIN_PASSWORD}"
  set_secret "${repo}" CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY "${CODESIGN_IDENTITY}"
  if [[ "${notary_mode}" == "api_key" ]]; then
    set_secret "${repo}" CONTEXTLATTICE_MACOS_NOTARY_KEY_P8_BASE64 "${notary_key_b64}"
    set_secret "${repo}" CONTEXTLATTICE_MACOS_NOTARY_KEY_ID "${NOTARY_KEY_ID}"
    if [[ -n "${NOTARY_ISSUER_ID}" ]]; then
      set_secret "${repo}" CONTEXTLATTICE_MACOS_NOTARY_ISSUER_ID "${NOTARY_ISSUER_ID}"
    fi
  else
    set_secret "${repo}" CONTEXTLATTICE_MACOS_NOTARY_APPLE_ID "${NOTARY_APPLE_ID}"
    set_secret "${repo}" CONTEXTLATTICE_MACOS_NOTARY_TEAM_ID "${NOTARY_TEAM_ID}"
    set_secret "${repo}" CONTEXTLATTICE_MACOS_NOTARY_PASSWORD "${NOTARY_PASSWORD}"
  fi
  if [[ "${REQUIRED_GATES}" == "1" ]]; then
    set_variable "${repo}" CONTEXTLATTICE_MACOS_SIGNING_REQUIRED true
    set_variable "${repo}" CONTEXTLATTICE_MACOS_NOTARIZATION_REQUIRED true
  fi
done
