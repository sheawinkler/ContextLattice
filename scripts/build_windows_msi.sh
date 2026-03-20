#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist}"
PKG_DIR="${ROOT_DIR}/packaging/windows"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-msi.XXXXXX")"
STAGE_DIR="${TMP_DIR}/stage"
WIX_IMAGE="${WIX_IMAGE:-marco98/msitools:latest}"
WIX_PLATFORM="${WIX_PLATFORM:-linux/amd64}"
MSI_ARCH="${MSI_ARCH:-x64}"
MSI_NAME="${MSI_NAME:-ContextLattice-windows-${MSI_ARCH}.msi}"
MSI_PATH="${DIST_DIR}/${MSI_NAME}"
VERSION_RAW="${MSI_VERSION:-$(git -C "${ROOT_DIR}" describe --tags --abbrev=0 2>/dev/null || echo "0.0.0")}"
VERSION="$(echo "${VERSION_RAW}" | sed -E 's/^[^0-9]*([0-9]+\.[0-9]+\.[0-9]+).*/\1/')"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

if [[ ! -d "${PKG_DIR}" ]]; then
  echo "ERROR: missing packaging directory: ${PKG_DIR}" >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: docker is required to build MSI from macOS/Linux." >&2
  exit 1
fi

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "WARN: could not parse semver from '${VERSION_RAW}', falling back to 0.0.0"
  VERSION="0.0.0"
fi

mkdir -p "${DIST_DIR}" "${STAGE_DIR}"

cp "${PKG_DIR}/ContextLattice-Install.cmd" "${STAGE_DIR}/"
cp "${PKG_DIR}/ContextLattice-Monitor.cmd" "${STAGE_DIR}/"
cp "${PKG_DIR}/Install-ContextLattice.ps1" "${STAGE_DIR}/"
cp "${PKG_DIR}/Monitor-ContextLattice.ps1" "${STAGE_DIR}/"
cp "${PKG_DIR}/README.txt" "${STAGE_DIR}/"
cp "${PKG_DIR}/contextlattice.wxs" "${STAGE_DIR}/"

if [[ -f "${MSI_PATH}" ]]; then
  rm -f "${MSI_PATH}"
fi

docker run --rm \
  --platform "${WIX_PLATFORM}" \
  -v "${STAGE_DIR}:/work" \
  -w /work \
  "${WIX_IMAGE}" \
  wixl \
  -v \
  -a "${MSI_ARCH}" \
  -D "Version=${VERSION}" \
  -o "${MSI_NAME}" \
  contextlattice.wxs

mv "${STAGE_DIR}/${MSI_NAME}" "${MSI_PATH}"
echo "Built MSI: ${MSI_PATH}"
