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

source_epoch="$(git -C "${ROOT_DIR}" show -s --format=%ct "${RELEASE_COMMIT:-HEAD}")"
python3 - "${STAGE_DIR}" "${ARCHIVE_PATH}" "${source_epoch}" <<'PY'
from __future__ import annotations

import gzip
import io
import os
import stat
import sys
import tarfile
from pathlib import Path

stage = Path(sys.argv[1]).resolve()
destination = Path(sys.argv[2]).resolve()
epoch = int(sys.argv[3])
temporary = destination.with_suffix(destination.suffix + ".tmp")
root_name = "ContextLattice-linux-bootstrap"


def mode(path: Path) -> int:
    if path.is_dir():
        return 0o755
    return 0o755 if path.stat().st_mode & stat.S_IXUSR else 0o644


def add(tf: tarfile.TarFile, path: Path, name: str) -> None:
    if path.is_symlink():
        raise SystemExit(f"ERROR: Linux bundle contains a symlink: {path}")
    info = tarfile.TarInfo(name + ("/" if path.is_dir() else ""))
    info.uid = info.gid = 0
    info.uname = info.gname = "root"
    info.mtime = epoch
    info.mode = mode(path)
    if path.is_dir():
        info.type = tarfile.DIRTYPE
        tf.addfile(info)
        return
    data = path.read_bytes()
    info.size = len(data)
    tf.addfile(info, io.BytesIO(data))


destination.parent.mkdir(parents=True, exist_ok=True)
try:
    with temporary.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, compresslevel=9, mtime=epoch) as gz:
            with tarfile.open(fileobj=gz, mode="w", format=tarfile.GNU_FORMAT) as tf:
                add(tf, stage, root_name)
                for path in sorted(stage.rglob("*"), key=lambda item: item.relative_to(stage).as_posix()):
                    add(tf, path, f"{root_name}/{path.relative_to(stage).as_posix()}")
    os.replace(temporary, destination)
finally:
    temporary.unlink(missing_ok=True)
PY

echo "Built Linux bundle: ${ARCHIVE_PATH}"
