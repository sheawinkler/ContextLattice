#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "ERROR: macOS DMG packaging requires Darwin (hdiutil)." >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist}"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-dmg.XXXXXX")"
STAGE_DIR="${TMP_DIR}/ContextLattice"
PAYLOAD_BUILD_DIR="${TMP_DIR}/payload-build"
APP_NAME="ContextLattice"
DMG_NAME="${DMG_NAME:-ContextLattice-macOS-universal.dmg}"
DMG_PATH="${DIST_DIR}/${DMG_NAME}"
RELEASE_LANE="${RELEASE_LANE:-}"
MACOS_CODESIGN_IDENTITY="${CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY:-${PAID_MACOS_CODESIGN_IDENTITY:-}}"
MACOS_SIGN_APPS="${CONTEXTLATTICE_MACOS_SIGN_APPS:-auto}"
MACOS_SIGNING_REQUIRED="${CONTEXTLATTICE_MACOS_SIGNING_REQUIRED:-false}"
MACOS_CODESIGN_TIMESTAMP="${CONTEXTLATTICE_MACOS_CODESIGN_TIMESTAMP:-true}"

case "${RELEASE_LANE}" in
  paid|public) ;;
  *)
    echo "ERROR: RELEASE_LANE must be explicitly set to 'paid' or 'public'." >&2
    exit 1
    ;;
esac

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

mkdir -p "${DIST_DIR}" "${STAGE_DIR}"

PAYLOAD_OUT_DIR="${PAYLOAD_BUILD_DIR}" \
PAYLOAD_FORMATS="tar.gz" \
RELEASE_LANE="${RELEASE_LANE}" \
  bash "${ROOT_DIR}/scripts/build_release_payload.sh"

is_truthy() {
  case "${1:-}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

sign_app_bundle() {
  local app_path="$1"

  case "${MACOS_SIGN_APPS}" in
    never|false|FALSE|0|off|OFF)
      echo "Skipping app bundle signing for ${app_path}."
      return 0
      ;;
  esac

  if [[ -z "${MACOS_CODESIGN_IDENTITY}" ]]; then
    if is_truthy "${MACOS_SIGNING_REQUIRED}"; then
      echo "ERROR: macOS app signing is required but CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY is missing." >&2
      exit 1
    fi
    echo "Skipping app bundle signing for ${app_path}: no signing identity configured."
    return 0
  fi

  if ! command -v codesign >/dev/null 2>&1; then
    echo "ERROR: codesign is required to sign app bundle: ${app_path}" >&2
    exit 1
  fi

  local timestamp_args=()
  if is_truthy "${MACOS_CODESIGN_TIMESTAMP}" && [[ "${MACOS_CODESIGN_IDENTITY}" != "-" ]]; then
    timestamp_args=(--timestamp)
  fi

  codesign \
    --force \
    --deep \
    --options runtime \
    "${timestamp_args[@]}" \
    --sign "${MACOS_CODESIGN_IDENTITY}" \
    "${app_path}"
  codesign --verify --deep --strict --verbose=2 "${app_path}"
  echo "Signed app bundle: ${app_path}"
}

python3 "${ROOT_DIR}/scripts/release_installer_outer.py" stage \
  --root "${ROOT_DIR}" \
  --kind macos \
  --lane "${RELEASE_LANE}" \
  --release-tag "${RELEASE_TAG}" \
  --output "${STAGE_DIR}" >/dev/null
cp "${PAYLOAD_BUILD_DIR}/contextlattice-release.json" "${STAGE_DIR}/contextlattice-release.json"
mkdir -p "${STAGE_DIR}/ContextLattice.app/Contents/Resources/payload"
cp \
  "${PAYLOAD_BUILD_DIR}/contextlattice-payload.tar.gz" \
  "${PAYLOAD_BUILD_DIR}/contextlattice-payload.tar.gz.sha256" \
  "${PAYLOAD_BUILD_DIR}/contextlattice-release.json" \
  "${STAGE_DIR}/ContextLattice.app/Contents/Resources/payload/"

if [[ ! -s "${STAGE_DIR}/ContextLattice.app/Contents/Resources/payload/contextlattice-payload.tar.gz" ]]; then
  echo "ERROR: ${RELEASE_LANE} macOS payload is missing." >&2
  exit 1
fi

sign_app_bundle "${STAGE_DIR}/ContextLattice.app"
sign_app_bundle "${STAGE_DIR}/ContextLattice Monitoring.app"

if [[ -f "${DMG_PATH}" ]]; then
  rm -f "${DMG_PATH}"
fi

hdiutil create \
  -volname "${APP_NAME}" \
  -srcfolder "${STAGE_DIR}" \
  -ov \
  -format UDZO \
  "${DMG_PATH}" >/dev/null

echo "Built DMG: ${DMG_PATH}"
