#!/usr/bin/env bash
set -euo pipefail

SOURCE_DIR="${1:-$(pwd)}"
DEST_DIR="${2:-${SOURCE_DIR}/tmp/contextlattice-staging}"
MANIFEST_FILE="${3:-${SOURCE_DIR}/scripts/repo_curate_manifest.txt}"
EXCLUDE_FILE="${4:-${SOURCE_DIR}/scripts/repo_curate_exclude.txt}"

if [[ ! -f "${MANIFEST_FILE}" ]]; then
  echo "manifest not found: ${MANIFEST_FILE}" >&2
  exit 1
fi

if [[ ! -f "${EXCLUDE_FILE}" ]]; then
  echo "exclude list not found: ${EXCLUDE_FILE}" >&2
  exit 1
fi

rm -rf "${DEST_DIR}"
mkdir -p "${DEST_DIR}"

rsync -a \
  --delete \
  --files-from="${MANIFEST_FILE}" \
  --exclude-from="${EXCLUDE_FILE}" \
  "${SOURCE_DIR}/" "${DEST_DIR}/"

echo "staged curated repo at ${DEST_DIR}"
echo "manifest: ${MANIFEST_FILE}"
echo "exclude: ${EXCLUDE_FILE}"
