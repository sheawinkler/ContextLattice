#!/usr/bin/env bash
set -euo pipefail

is_truthy() {
  case "${1:-}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

ARTIFACT_PATH="${1:-dist/ContextLattice-macOS-universal.dmg}"
SIGNING_REQUIRED="${CONTEXTLATTICE_MACOS_SIGNING_REQUIRED:-false}"
NOTARIZATION_REQUIRED="${CONTEXTLATTICE_MACOS_NOTARIZATION_REQUIRED:-false}"
CODESIGN_IDENTITY="${CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY:-${PAID_MACOS_CODESIGN_IDENTITY:-}}"
SIGNING_AVAILABLE="${CONTEXTLATTICE_MACOS_SIGNING_AVAILABLE:-auto}"
NOTARY_PROFILE="${CONTEXTLATTICE_MACOS_NOTARY_KEYCHAIN_PROFILE:-${PAID_MACOS_NOTARY_KEYCHAIN_PROFILE:-}}"
NOTARY_APPLE_ID="${CONTEXTLATTICE_MACOS_NOTARY_APPLE_ID:-${PAID_MACOS_NOTARY_APPLE_ID:-}}"
NOTARY_TEAM_ID="${CONTEXTLATTICE_MACOS_NOTARY_TEAM_ID:-${PAID_MACOS_NOTARY_TEAM_ID:-}}"
NOTARY_PASSWORD="${CONTEXTLATTICE_MACOS_NOTARY_PASSWORD:-${PAID_MACOS_NOTARY_PASSWORD:-}}"
CODESIGN_TIMESTAMP="${CONTEXTLATTICE_MACOS_CODESIGN_TIMESTAMP:-true}"
SIGNING_IMPORT_SKIPPED=0

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
elif [[ -n "${NOTARY_APPLE_ID}" && -n "${NOTARY_TEAM_ID}" && -n "${NOTARY_PASSWORD}" ]]; then
  notary_args=(--apple-id "${NOTARY_APPLE_ID}" --team-id "${NOTARY_TEAM_ID}" --password "${NOTARY_PASSWORD}")
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
