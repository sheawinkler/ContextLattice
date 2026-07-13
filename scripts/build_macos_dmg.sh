#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "ERROR: macOS DMG packaging requires Darwin (hdiutil)." >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist}"
PKG_DIR="${ROOT_DIR}/packaging/linux"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-dmg.XXXXXX")"
STAGE_DIR="${TMP_DIR}/ContextLattice"
PAYLOAD_BUILD_DIR="${TMP_DIR}/payload-build"
APP_NAME="ContextLattice"
DMG_NAME="${DMG_NAME:-ContextLattice-macOS-universal.dmg}"
DMG_PATH="${DIST_DIR}/${DMG_NAME}"
RELEASE_LANE="${RELEASE_LANE:-public}"
APP_BUNDLE_VERSION="${APP_BUNDLE_VERSION:-${RELEASE_TAG:-${GITHUB_REF_NAME:-0.0.0}}}"
APP_BUNDLE_VERSION="${APP_BUNDLE_VERSION#v}"
APP_BUNDLE_VERSION="${APP_BUNDLE_VERSION%%[-+]*}"
MIN_MACOS_VERSION="${MIN_MACOS_VERSION:-13.0}"
MACOS_CODESIGN_IDENTITY="${CONTEXTLATTICE_MACOS_CODESIGN_IDENTITY:-}"
MACOS_SIGN_APPS="${CONTEXTLATTICE_MACOS_SIGN_APPS:-auto}"
MACOS_SIGNING_REQUIRED="${CONTEXTLATTICE_MACOS_SIGNING_REQUIRED:-false}"
MACOS_CODESIGN_TIMESTAMP="${CONTEXTLATTICE_MACOS_CODESIGN_TIMESTAMP:-true}"

[[ "${RELEASE_LANE}" == "public" ]] || {
  echo "ERROR: the public repository builds only RELEASE_LANE=public DMGs." >&2
  exit 1
}

if [[ ! "${APP_BUNDLE_VERSION}" =~ ^[0-9]+(\.[0-9]+){0,2}$ ]]; then
  APP_BUNDLE_VERSION="0.0.0"
fi

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

write_app_bundle() {
  local app_path="$1"
  local display_name="$2"
  local bundle_id="$3"
  local executable_name="$4"
  local launcher_path="$5"
  local launcher_name
  launcher_name="$(basename "${launcher_path}")"

  mkdir -p "${app_path}/Contents/MacOS" "${app_path}/Contents/Resources"
  cp "${launcher_path}" "${app_path}/Contents/Resources/${launcher_name}"
  chmod +x "${app_path}/Contents/Resources/${launcher_name}"

  cat > "${app_path}/Contents/Info.plist" <<EOF_PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleDevelopmentRegion</key>
  <string>en</string>
  <key>CFBundleExecutable</key>
  <string>${executable_name}</string>
  <key>CFBundleIdentifier</key>
  <string>${bundle_id}</string>
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>CFBundleName</key>
  <string>${display_name}</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>${APP_BUNDLE_VERSION}</string>
  <key>CFBundleVersion</key>
  <string>${APP_BUNDLE_VERSION}</string>
  <key>LSMinimumSystemVersion</key>
  <string>${MIN_MACOS_VERSION}</string>
  <key>LSUIElement</key>
  <false/>
  <key>NSHighResolutionCapable</key>
  <true/>
</dict>
</plist>
EOF_PLIST

  cat > "${app_path}/Contents/MacOS/${executable_name}" <<EOF_EXEC
#!/usr/bin/env bash
set -euo pipefail

APP_ROOT="\$(cd "\$(dirname "\${BASH_SOURCE[0]}")/.." && pwd)"
LAUNCH_SCRIPT="\${APP_ROOT}/Resources/${launcher_name}"

if [[ ! -x "\${LAUNCH_SCRIPT}" ]]; then
  chmod +x "\${LAUNCH_SCRIPT}" 2>/dev/null || true
fi

if [[ -t 1 ]]; then
  exec "\${LAUNCH_SCRIPT}"
fi

if command -v osascript >/dev/null 2>&1; then
  /usr/bin/osascript \
    -e 'on run argv' \
    -e 'set launcherPath to item 1 of argv' \
    -e 'tell application "Terminal"' \
    -e 'activate' \
    -e 'do script quoted form of launcherPath' \
    -e 'end tell' \
    -e 'end run' \
    "\${LAUNCH_SCRIPT}" >/dev/null
else
  exec "\${LAUNCH_SCRIPT}"
fi
EOF_EXEC
  chmod +x "${app_path}/Contents/MacOS/${executable_name}"
}

sed "s/@RELEASE_LANE@/${RELEASE_LANE}/g" \
  "${PKG_DIR}/ContextLattice-Install.sh" > "${STAGE_DIR}/ContextLattice.command"

cat > "${STAGE_DIR}/Monitoring.command" <<'EOF_MONITOR'
#!/usr/bin/env bash
set -euo pipefail

TARGET_DIR="${TARGET_DIR:-$HOME/ContextLattice}"
if [[ ! -d "${TARGET_DIR}" ]]; then
  echo "ERROR: ${TARGET_DIR} not found. Run ContextLattice first." >&2
  exit 1
fi

cd "${TARGET_DIR}"
if [[ -x ./scripts/open_monitoring.sh ]]; then
  ./scripts/open_monitoring.sh
else
  echo "ERROR: monitoring script is absent from the installed payload." >&2
  exit 1
fi
EOF_MONITOR

cat > "${STAGE_DIR}/README.txt" <<EOF_README
ContextLattice macOS Release DMG
================================

This DMG contains a ${RELEASE_LANE} lane payload from ${RELEASE_TAG}.
No repository clone or pull is used during installation.

Included:
- ContextLattice.app and ContextLattice.command: verify, extract/update, and launch
- ContextLattice Monitoring.app and Monitoring.command: local health/status tools
- contextlattice-release.json: bounded lane/tag/commit identity

For deterministic extraction without Docker or network:
  ./ContextLattice.command --extract-only --install-dir /tmp/contextlattice

Atomic updates preserve .env, .data, data, and backups inside the install
directory. Modified legacy checkouts and unmanaged non-empty directories are
refused instead of overwritten.
EOF_README

chmod +x "${STAGE_DIR}/ContextLattice.command" "${STAGE_DIR}/Monitoring.command"
cp "${PAYLOAD_BUILD_DIR}/contextlattice-release.json" "${STAGE_DIR}/contextlattice-release.json"

write_app_bundle \
  "${STAGE_DIR}/ContextLattice.app" \
  "ContextLattice" \
  "io.contextlattice.ContextLattice" \
  "ContextLattice" \
  "${STAGE_DIR}/ContextLattice.command"
mkdir -p "${STAGE_DIR}/ContextLattice.app/Contents/Resources/payload"
cp \
  "${PAYLOAD_BUILD_DIR}/contextlattice-payload.tar.gz" \
  "${PAYLOAD_BUILD_DIR}/contextlattice-payload.tar.gz.sha256" \
  "${PAYLOAD_BUILD_DIR}/contextlattice-release.json" \
  "${STAGE_DIR}/ContextLattice.app/Contents/Resources/payload/"

write_app_bundle \
  "${STAGE_DIR}/ContextLattice Monitoring.app" \
  "ContextLattice Monitoring" \
  "io.contextlattice.ContextLatticeMonitoring" \
  "ContextLatticeMonitoring" \
  "${STAGE_DIR}/Monitoring.command"

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
