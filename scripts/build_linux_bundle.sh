#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist}"
PKG_DIR="${ROOT_DIR}/packaging/linux"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-linux-bundle.XXXXXX")"
STAGE_DIR="${TMP_DIR}/ContextLattice-linux-bootstrap"
ARCHIVE_NAME="${LINUX_BUNDLE_NAME:-ContextLattice-linux-bootstrap.tar.gz}"
ARCHIVE_PATH="${DIST_DIR}/${ARCHIVE_NAME}"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

if [[ ! -d "${PKG_DIR}" ]]; then
  echo "ERROR: missing packaging directory: ${PKG_DIR}" >&2
  exit 1
fi

mkdir -p "${DIST_DIR}" "${STAGE_DIR}"

cp "${PKG_DIR}/ContextLattice-Install.sh" "${STAGE_DIR}/"
cp "${PKG_DIR}/ContextLattice-Monitor.sh" "${STAGE_DIR}/"
cp "${PKG_DIR}/README.txt" "${STAGE_DIR}/"

chmod +x "${STAGE_DIR}/ContextLattice-Install.sh" "${STAGE_DIR}/ContextLattice-Monitor.sh"

if [[ -f "${ARCHIVE_PATH}" ]]; then
  rm -f "${ARCHIVE_PATH}"
fi

tar -C "${TMP_DIR}" -czf "${ARCHIVE_PATH}" "ContextLattice-linux-bootstrap"

echo "Built Linux bundle: ${ARCHIVE_PATH}"
