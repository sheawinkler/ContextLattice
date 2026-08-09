#!/usr/bin/env bash
set -euo pipefail
export COPYFILE_DISABLE=1

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist}"
PKG_DIR="${ROOT_DIR}/packaging/linux"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-linux-bundle.XXXXXX")"
STAGE_DIR="${TMP_DIR}/ContextLattice-linux-bootstrap"
PAYLOAD_BUILD_DIR="${TMP_DIR}/payload-build"
ARCHIVE_NAME="${LINUX_BUNDLE_NAME:-ContextLattice-linux-bootstrap.tar.gz}"
ARCHIVE_PATH="${DIST_DIR}/${ARCHIVE_NAME}"
ARCHIVE_CANDIDATE=""
RELEASE_LANE="${RELEASE_LANE:-}"

cleanup() {
	if [[ -n "${ARCHIVE_CANDIDATE}" ]]; then
		rm -f "${ARCHIVE_CANDIDATE}"
	fi
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

if [[ ! -d "${PKG_DIR}" ]]; then
  echo "ERROR: missing packaging directory: ${PKG_DIR}" >&2
  exit 1
fi

mkdir -p "${DIST_DIR}" "${STAGE_DIR}"

case "${RELEASE_LANE}" in
  paid|public) ;;
  *)
    echo "ERROR: RELEASE_LANE must be explicitly set to 'paid' or 'public'." >&2
    exit 1
    ;;
esac

PAYLOAD_OUT_DIR="${PAYLOAD_BUILD_DIR}" \
PAYLOAD_FORMATS="tar.gz" \
RELEASE_LANE="${RELEASE_LANE}" \
  bash "${ROOT_DIR}/scripts/build_release_payload.sh"

python3 "${ROOT_DIR}/scripts/release_installer_outer.py" stage \
  --root "${ROOT_DIR}" \
  --kind linux \
  --lane "${RELEASE_LANE}" \
  --release-tag "${RELEASE_TAG}" \
  --output "${STAGE_DIR}" >/dev/null
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

python3 "${ROOT_DIR}/scripts/release_installer_outer.py" validate \
  --root "${ROOT_DIR}" \
  --kind linux \
  --lane "${RELEASE_LANE}" \
  --release-tag "${RELEASE_TAG}" \
  --actual-root "${STAGE_DIR}" >/dev/null

ARCHIVE_CANDIDATE="$(mktemp "${DIST_DIR}/.${ARCHIVE_NAME}.candidate.XXXXXX")"
python3 "${ROOT_DIR}/scripts/release_installer_outer.py" build-linux-archive \
  --stage "${STAGE_DIR}" \
  --archive "${ARCHIVE_CANDIDATE}" >/dev/null

python3 "${ROOT_DIR}/scripts/release_installer_outer.py" validate-linux-archive \
  --root "${ROOT_DIR}" \
  --lane "${RELEASE_LANE}" \
  --release-tag "${RELEASE_TAG}" \
  --archive "${ARCHIVE_CANDIDATE}" >/dev/null

mv -f "${ARCHIVE_CANDIDATE}" "${ARCHIVE_PATH}"
ARCHIVE_CANDIDATE=""

echo "Built Linux bundle: ${ARCHIVE_PATH}"
