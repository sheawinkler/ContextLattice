#!/usr/bin/env bash
set -euo pipefail

is_truthy() {
  case "${1:-}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

base64_decode() {
  if base64 --decode >/dev/null 2>&1 <<<"YQ=="; then
    base64 --decode
  else
    base64 -D
  fi
}

ARTIFACT_PATH="${1:-dist/ContextLattice-macOS-universal.dmg}"
SIGNING_REQUIRED="${CONTEXTLATTICE_MACOS_SIGNING_REQUIRED:-false}"
NOTARIZATION_REQUIRED="${CONTEXTLATTICE_MACOS_NOTARIZATION_REQUIRED:-false}"
CODESIGN_IDENTITY="${CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY:-}"
SIGNING_AVAILABLE="${CONTEXTLATTICE_MACOS_SIGNING_AVAILABLE:-auto}"
NOTARY_PROFILE="${CONTEXTLATTICE_MACOS_NOTARY_KEYCHAIN_PROFILE:-}"
NOTARY_KEY_P8_BASE64="${CONTEXTLATTICE_MACOS_NOTARY_KEY_P8_BASE64:-}"
NOTARY_KEY_P8_PATH="${CONTEXTLATTICE_MACOS_NOTARY_KEY_P8_PATH:-}"
NOTARY_KEY_ID="${CONTEXTLATTICE_MACOS_NOTARY_KEY_ID:-}"
NOTARY_ISSUER_ID="${CONTEXTLATTICE_MACOS_NOTARY_ISSUER_ID:-}"
NOTARY_APPLE_ID="${CONTEXTLATTICE_MACOS_NOTARY_APPLE_ID:-}"
NOTARY_TEAM_ID="${CONTEXTLATTICE_MACOS_NOTARY_TEAM_ID:-}"
NOTARY_PASSWORD="${CONTEXTLATTICE_MACOS_NOTARY_PASSWORD:-}"
CODESIGN_TIMESTAMP="${CONTEXTLATTICE_MACOS_CODESIGN_TIMESTAMP:-true}"
SIGNING_IMPORT_SKIPPED=0
TMP_KEY_PATH=""

cleanup() {
  if [[ -n "${TMP_KEY_PATH}" ]]; then
    rm -f "${TMP_KEY_PATH}"
  fi
}
trap cleanup EXIT

if [[ "$(uname -s)" != "Darwin" ]]; then
  if is_truthy "${SIGNING_REQUIRED}" || is_truthy "${NOTARIZATION_REQUIRED}"; then
    echo "ERROR: macOS signing/notarization requires Darwin." >&2
    exit 1
  fi
  echo "macOS signing/notarization skipped: host is not Darwin."
  exit 0
fi

if [[ ! -f "${ARTIFACT_PATH}" ]]; then
  echo "ERROR: release artifact not found: ${ARTIFACT_PATH}" >&2
  exit 1
fi

if [[ "${SIGNING_AVAILABLE}" == "false" && -n "${CODESIGN_IDENTITY}" ]]; then
  if is_truthy "${SIGNING_REQUIRED}"; then
    echo "ERROR: macOS signing identity was configured but certificate import did not complete." >&2
    exit 1
  fi
  echo "macOS artifact signing skipped: signing identity was not imported."
  CODESIGN_IDENTITY=""
  SIGNING_IMPORT_SKIPPED=1
fi

if [[ -n "${CODESIGN_IDENTITY}" ]]; then
  timestamp_args=()
  if is_truthy "${CODESIGN_TIMESTAMP}" && [[ "${CODESIGN_IDENTITY}" != "-" ]]; then
    timestamp_args=(--timestamp)
  fi
  codesign --force --sign "${CODESIGN_IDENTITY}" "${timestamp_args[@]}" "${ARTIFACT_PATH}"
  codesign --verify --verbose "${ARTIFACT_PATH}"
  echo "Signed macOS release artifact: ${ARTIFACT_PATH}"
elif is_truthy "${SIGNING_REQUIRED}"; then
  echo "ERROR: macOS signing is required but CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY is missing." >&2
  exit 1
else
  if [[ "${SIGNING_IMPORT_SKIPPED}" != "1" ]]; then
    echo "macOS artifact signing skipped: no signing identity configured."
  fi
fi

notary_args=()
if [[ -n "${NOTARY_PROFILE}" ]]; then
  notary_args=(--keychain-profile "${NOTARY_PROFILE}")
elif [[ -n "${NOTARY_KEY_P8_BASE64}" || -n "${NOTARY_KEY_P8_PATH}" || -n "${NOTARY_KEY_ID}" || -n "${NOTARY_ISSUER_ID}" ]]; then
  if [[ -z "${NOTARY_KEY_ID}" ]]; then
    echo "ERROR: App Store Connect API key notarization requires CONTEXTLATTICE_MACOS_NOTARY_KEY_ID." >&2
    exit 1
  fi
  if [[ -n "${NOTARY_KEY_P8_BASE64}" ]]; then
    TMP_KEY_PATH="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/contextlattice-notary-${NOTARY_KEY_ID}.p8"
    printf '%s' "${NOTARY_KEY_P8_BASE64}" | base64_decode >"${TMP_KEY_PATH}"
    chmod 0600 "${TMP_KEY_PATH}"
    NOTARY_KEY_P8_PATH="${TMP_KEY_PATH}"
  fi
  if [[ -z "${NOTARY_KEY_P8_PATH}" || ! -f "${NOTARY_KEY_P8_PATH}" ]]; then
    echo "ERROR: App Store Connect API key notarization requires CONTEXTLATTICE_MACOS_NOTARY_KEY_P8_BASE64 or CONTEXTLATTICE_MACOS_NOTARY_KEY_P8_PATH." >&2
    exit 1
  fi
  notary_args=(--key "${NOTARY_KEY_P8_PATH}" --key-id "${NOTARY_KEY_ID}")
  if [[ -n "${NOTARY_ISSUER_ID}" ]]; then
    notary_args+=(--issuer "${NOTARY_ISSUER_ID}")
  fi
elif [[ -n "${NOTARY_APPLE_ID}" && -n "${NOTARY_TEAM_ID}" && -n "${NOTARY_PASSWORD}" ]]; then
  notary_args=(--apple-id "${NOTARY_APPLE_ID}" --team-id "${NOTARY_TEAM_ID}" --password "${NOTARY_PASSWORD}")
elif [[ -n "${NOTARY_APPLE_ID}" || -n "${NOTARY_TEAM_ID}" || -n "${NOTARY_PASSWORD}" ]]; then
  echo "ERROR: Apple ID notarization requires CONTEXTLATTICE_MACOS_NOTARY_APPLE_ID, CONTEXTLATTICE_MACOS_NOTARY_TEAM_ID, and CONTEXTLATTICE_MACOS_NOTARY_PASSWORD." >&2
  exit 1
fi

if (( ${#notary_args[@]} == 0 )); then
  if is_truthy "${NOTARIZATION_REQUIRED}"; then
    echo "ERROR: macOS notarization is required but no notary profile or Apple ID credentials are configured." >&2
    exit 1
  fi
  echo "macOS notarization skipped: no notarization credentials configured."
  exit 0
fi

xcrun notarytool submit "${ARTIFACT_PATH}" "${notary_args[@]}" --wait
xcrun stapler staple "${ARTIFACT_PATH}"

if ! spctl --assess --type open --context context:primary-signature -v "${ARTIFACT_PATH}"; then
  echo "ERROR: Gatekeeper assessment failed for notarized artifact: ${ARTIFACT_PATH}" >&2
  exit 1
fi

echo "Notarized and stapled macOS release artifact: ${ARTIFACT_PATH}"
