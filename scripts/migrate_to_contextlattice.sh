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

while IFS= read -r raw_path || [[ -n "${raw_path}" ]]; do
  path="${raw_path%%#*}"
  path="${path%"${path##*[![:space:]]}"}"
  path="${path#"${path%%[![:space:]]*}"}"
  path="${path%/}"

  if [[ -z "${path}" ]]; then
    continue
  fi

  src_path="${SOURCE_DIR}/${path}"
  if [[ ! -e "${src_path}" ]]; then
    echo "warn: manifest path missing, skipping: ${path}" >&2
    continue
  fi

  parent_dir="$(dirname "${path}")"
  mkdir -p "${DEST_DIR}/${parent_dir}"

  rsync -a \
    --exclude-from="${EXCLUDE_FILE}" \
    "${src_path}" "${DEST_DIR}/${parent_dir}/"
done < "${MANIFEST_FILE}"

echo "staged curated repo at ${DEST_DIR}"
echo "manifest: ${MANIFEST_FILE}"
echo "exclude: ${EXCLUDE_FILE}"
