#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist}"
PKG_DIR="${ROOT_DIR}/packaging/linux"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-linux-bundle.XXXXXX")"
STAGE_DIR="${TMP_DIR}/ContextLattice-linux-bootstrap"
PAYLOAD_BUILD_DIR="${TMP_DIR}/payload-build"
ARCHIVE_NAME="${LINUX_BUNDLE_NAME:-ContextLattice-linux-bootstrap.tar.gz}"
ARCHIVE_PATH="${DIST_DIR}/${ARCHIVE_NAME}"
RELEASE_LANE="${RELEASE_LANE:-public}"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

if [[ ! -d "${PKG_DIR}" ]]; then
  echo "ERROR: missing packaging directory: ${PKG_DIR}" >&2
  exit 1
fi

mkdir -p "${DIST_DIR}" "${STAGE_DIR}"

[[ "${RELEASE_LANE}" == "public" ]] || {
  echo "ERROR: the public repository builds only RELEASE_LANE=public bundles." >&2
  exit 1
}

PAYLOAD_OUT_DIR="${PAYLOAD_BUILD_DIR}" \
PAYLOAD_FORMATS="tar.gz" \
RELEASE_LANE="${RELEASE_LANE}" \
  bash "${ROOT_DIR}/scripts/build_release_payload.sh"

sed "s/@RELEASE_LANE@/${RELEASE_LANE}/g" \
  "${PKG_DIR}/ContextLattice-Install.sh" > "${STAGE_DIR}/ContextLattice-Install.sh"
cp "${PKG_DIR}/ContextLattice-Monitor.sh" "${STAGE_DIR}/"
cp "${PKG_DIR}/README.txt" "${STAGE_DIR}/"
mkdir -p "${STAGE_DIR}/payload"
cp \
  "${PAYLOAD_BUILD_DIR}/contextlattice-payload.tar.gz" \
  "${PAYLOAD_BUILD_DIR}/contextlattice-payload.tar.gz.sha256" \
  "${PAYLOAD_BUILD_DIR}/contextlattice-release.json" \
  "${STAGE_DIR}/payload/"

if [[ ! -s "${STAGE_DIR}/payload/contextlattice-payload.tar.gz" ]]; then
  echo "ERROR: ${RELEASE_LANE} Linux bundle payload is missing." >&2
  exit 1
fi

chmod +x "${STAGE_DIR}/ContextLattice-Install.sh" "${STAGE_DIR}/ContextLattice-Monitor.sh"

if [[ -f "${ARCHIVE_PATH}" ]]; then
  rm -f "${ARCHIVE_PATH}"
fi

tar -C "${TMP_DIR}" -czf "${ARCHIVE_PATH}" "ContextLattice-linux-bootstrap"

echo "Built Linux bundle: ${ARCHIVE_PATH}"
