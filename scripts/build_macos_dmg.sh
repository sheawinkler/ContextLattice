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
APP_NAME="ContextLattice"
DMG_NAME="${DMG_NAME:-ContextLattice-macOS-universal.dmg}"
DMG_PATH="${DIST_DIR}/${DMG_NAME}"
REPO_URL="${REPO_URL:-https://github.com/sheawinkler/ContextLattice.git}"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

mkdir -p "${DIST_DIR}" "${STAGE_DIR}"

cat > "${STAGE_DIR}/ContextLattice.command" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/sheawinkler/ContextLattice.git}"
TARGET_DIR="${TARGET_DIR:-$HOME/ContextLattice}"

echo "== ContextLattice installer =="
echo "Repo: ${REPO_URL}"
echo "Target: ${TARGET_DIR}"

if ! command -v git >/dev/null 2>&1; then
  echo "ERROR: git is required. Install Xcode Command Line Tools first." >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: Docker is required. Install Docker Desktop, then rerun this installer." >&2
  exit 1
fi

if [[ ! -d "${TARGET_DIR}/.git" ]]; then
  git clone "${REPO_URL}" "${TARGET_DIR}"
else
  git -C "${TARGET_DIR}" pull --ff-only || true
fi

if ! command -v gmake >/dev/null 2>&1; then
  if command -v brew >/dev/null 2>&1; then
    brew install make jq ripgrep
  else
    echo "ERROR: gmake is required. Install Homebrew then run: brew install make jq ripgrep" >&2
    exit 1
  fi
fi

cd "${TARGET_DIR}"
QUICKSTART_PROFILE_DEFAULT="${QUICKSTART_PROFILE_DEFAULT:-lite}" gmake quickstart

if [[ -x ./scripts/open_monitoring.sh ]]; then
  ./scripts/open_monitoring.sh || true
fi

echo "ContextLattice installed. Use the Monitoring.command launcher for health checks."
EOF

cat > "${STAGE_DIR}/Monitoring.command" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

TARGET_DIR="${TARGET_DIR:-$HOME/ContextLattice}"
if [[ ! -d "${TARGET_DIR}" ]]; then
  echo "ERROR: ${TARGET_DIR} not found. Run ContextLattice.command first." >&2
  exit 1
fi

cd "${TARGET_DIR}"
if [[ -x ./scripts/open_monitoring.sh ]]; then
  ./scripts/open_monitoring.sh
else
  echo "Monitoring script missing. Re-pull repo and retry." >&2
  exit 1
fi
EOF

cat > "${STAGE_DIR}/README.txt" <<EOF
ContextLattice macOS DMG Bootstrap
=================================

This DMG installs the local ContextLattice stack through the same public repo
used by technical users, but in a lower-friction launcher format.

Included:
- ContextLattice.command  : clone/update + gmake quickstart
- Monitoring.command      : open dashboard + health/status checks

Requirements:
- Docker Desktop installed and running
- git (Xcode command line tools)
- Homebrew (recommended; installer will use it if gmake is missing)

Repo:
${REPO_URL}
EOF

chmod +x "${STAGE_DIR}/ContextLattice.command" "${STAGE_DIR}/Monitoring.command"

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
