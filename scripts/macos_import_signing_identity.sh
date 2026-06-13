#!/usr/bin/env bash
set -euo pipefail

is_truthy() {
  case "${1:-}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

set_github_env() {
  local key="$1"
  local value="$2"
  if [[ -n "${GITHUB_ENV:-}" ]]; then
    printf '%s=%s\n' "$key" "$value" >>"${GITHUB_ENV}"
  fi
}

base64_decode() {
  if base64 --decode >/dev/null 2>&1 <<<"YQ=="; then
    base64 --decode
  else
    base64 -D
  fi
}

REQUIRED="${CONTEXTLATTICE_MACOS_SIGNING_REQUIRED:-false}"

if [[ "$(uname -s)" != "Darwin" ]]; then
  if is_truthy "${REQUIRED}"; then
    echo "ERROR: macOS signing identity import requires Darwin." >&2
    exit 1
  fi
  echo "macOS signing identity import skipped: host is not Darwin."
  set_github_env CONTEXTLATTICE_MACOS_SIGNING_AVAILABLE false
  exit 0
fi

CERT_B64="${CONTEXTLATTICE_MACOS_CERT_P12_BASE64:-${PAID_MACOS_CERT_P12_BASE64:-}}"
CERT_PASSWORD="${CONTEXTLATTICE_MACOS_CERT_P12_PASSWORD:-${PAID_MACOS_CERT_P12_PASSWORD:-}}"
KEYCHAIN_PASSWORD="${CONTEXTLATTICE_MACOS_KEYCHAIN_PASSWORD:-${PAID_MACOS_KEYCHAIN_PASSWORD:-}}"
CODESIGN_IDENTITY="${CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY:-${PAID_MACOS_CODESIGN_IDENTITY:-}}"

missing=()
[[ -n "${CERT_B64}" ]] || missing+=("CONTEXTLATTICE_MACOS_CERT_P12_BASE64")
[[ -n "${CERT_PASSWORD}" ]] || missing+=("CONTEXTLATTICE_MACOS_CERT_P12_PASSWORD")
[[ -n "${KEYCHAIN_PASSWORD}" ]] || missing+=("CONTEXTLATTICE_MACOS_KEYCHAIN_PASSWORD")
[[ -n "${CODESIGN_IDENTITY}" ]] || missing+=("CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY")

if (( ${#missing[@]} > 0 )); then
  if is_truthy "${REQUIRED}"; then
    echo "ERROR: macOS signing is required but these env vars/secrets are missing: ${missing[*]}" >&2
    exit 1
  fi
  echo "macOS signing identity import skipped: signing secrets are incomplete."
  set_github_env CONTEXTLATTICE_MACOS_SIGNING_AVAILABLE false
  exit 0
fi

TMP_ROOT="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
CERT_PATH="${TMP_ROOT}/contextlattice-codesign.p12"
KEYCHAIN_PATH="${CONTEXTLATTICE_MACOS_KEYCHAIN_PATH:-${TMP_ROOT}/contextlattice-build.keychain-db}"

printf '%s' "${CERT_B64}" | base64_decode >"${CERT_PATH}"
chmod 0600 "${CERT_PATH}"

if [[ -f "${KEYCHAIN_PATH}" ]]; then
  security delete-keychain "${KEYCHAIN_PATH}" >/dev/null 2>&1 || true
fi

security create-keychain -p "${KEYCHAIN_PASSWORD}" "${KEYCHAIN_PATH}"
security set-keychain-settings -lut 21600 "${KEYCHAIN_PATH}"
security unlock-keychain -p "${KEYCHAIN_PASSWORD}" "${KEYCHAIN_PATH}"
security import "${CERT_PATH}" -P "${CERT_PASSWORD}" -A -t cert -f pkcs12 -k "${KEYCHAIN_PATH}"
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "${KEYCHAIN_PASSWORD}" "${KEYCHAIN_PATH}" >/dev/null
security list-keychains -d user -s "${KEYCHAIN_PATH}" login.keychain-db
security default-keychain -d user -s "${KEYCHAIN_PATH}"

if ! security find-identity -v -p codesigning "${KEYCHAIN_PATH}" | grep -F "${CODESIGN_IDENTITY}" >/dev/null; then
  echo "ERROR: imported keychain does not expose the requested signing identity." >&2
  exit 1
fi

set_github_env CONTEXTLATTICE_MACOS_SIGNING_AVAILABLE true
set_github_env CONTEXTLATTICE_MACOS_KEYCHAIN_PATH "${KEYCHAIN_PATH}"
set_github_env CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY "${CODESIGN_IDENTITY}"

echo "macOS signing identity imported: ${CODESIGN_IDENTITY}"
