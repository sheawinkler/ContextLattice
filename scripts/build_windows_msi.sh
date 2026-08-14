#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist}"
PKG_DIR="${ROOT_DIR}/packaging/windows"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-msi.XXXXXX")"
STAGE_DIR="${TMP_DIR}/stage"
PAYLOAD_BUILD_DIR="${TMP_DIR}/payload-build"
readonly REVIEWED_WIX_IMAGE="marco98/msitools@sha256:0ac5297e0691e6768e1de4d7bdecef376ecdbff41c4cd7d4f3b55c5e7d42c48e"
WIX_IMAGE="${WIX_IMAGE:-${REVIEWED_WIX_IMAGE}}"
WIX_PLATFORM="${WIX_PLATFORM:-linux/amd64}"
MSI_ARCH="${MSI_ARCH:-x64}"
MSI_NAME="${MSI_NAME:-ContextLattice-windows-${MSI_ARCH}.msi}"
MSI_PATH="${DIST_DIR}/${MSI_NAME}"
VERSION_RAW="${MSI_VERSION:-$(git -C "${ROOT_DIR}" describe --tags --abbrev=0 2>/dev/null || echo "0.0.0")}"
VERSION="$(echo "${VERSION_RAW}" | sed -E 's/^[^0-9]*([0-9]+\.[0-9]+\.[0-9]+).*/\1/')"
RELEASE_LANE="${RELEASE_LANE:-}"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

if [[ ! -d "${PKG_DIR}" ]]; then
  echo "ERROR: missing packaging directory: ${PKG_DIR}" >&2
  exit 1
fi

case "${RELEASE_LANE}" in
  paid|public) ;;
  *)
    echo "ERROR: RELEASE_LANE must be explicitly set to 'paid' or 'public'." >&2
    exit 1
    ;;
esac

if [[ "${WIX_IMAGE}" != "${REVIEWED_WIX_IMAGE}" ]]; then
  echo "ERROR: WIX_IMAGE must remain pinned to the reviewed public digest." >&2
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

python3 "${ROOT_DIR}/scripts/release_installer_outer.py" stage \
  --root "${ROOT_DIR}" \
  --kind windows \
  --lane "${RELEASE_LANE}" \
  --release-tag "${RELEASE_TAG}" \
  --output "${STAGE_DIR}" >/dev/null
cp "${PKG_DIR}/contextlattice.wxs" "${STAGE_DIR}/"

PAYLOAD_OUT_DIR="${PAYLOAD_BUILD_DIR}" \
PAYLOAD_FORMATS="zip" \
RELEASE_LANE="${RELEASE_LANE}" \
  bash "${ROOT_DIR}/scripts/build_release_payload.sh"
mkdir -p "${STAGE_DIR}/payload"
cp \
  "${PAYLOAD_BUILD_DIR}/contextlattice-payload.zip" \
  "${PAYLOAD_BUILD_DIR}/contextlattice-payload.zip.sha256" \
  "${PAYLOAD_BUILD_DIR}/contextlattice-release.json" \
  "${STAGE_DIR}/payload/"

if [[ ! -s "${STAGE_DIR}/payload/contextlattice-payload.zip" ]]; then
  echo "ERROR: ${RELEASE_LANE} Windows MSI payload is missing." >&2
  exit 1
fi

if [[ -f "${MSI_PATH}" ]]; then
  rm -f "${MSI_PATH}"
fi

"${ROOT_DIR}/scripts/docker_anonymous_public.sh" run \
  --rm \
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
