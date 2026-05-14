#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_PROFILE="${CONTEXT_STORAGE_OPS_BUILD_PROFILE:-release}"
CARGO_BIN="${CARGO_BIN:-cargo}"
SUBCOMMAND="${1:-}"
shift || true

if [[ -n "${CONTEXT_STORAGE_OPS_BIN:-}" ]]; then
  exec "${CONTEXT_STORAGE_OPS_BIN}" "${SUBCOMMAND}" "$@"
fi

if command -v "${CARGO_BIN}" >/dev/null 2>&1; then
  if [[ "${BUILD_PROFILE}" == "release" ]]; then
    exec "${CARGO_BIN}" run --manifest-path "${ROOT_DIR}/crates/Cargo.toml" -p context_storage_ops --release -- "${SUBCOMMAND}" "$@"
  fi
  exec "${CARGO_BIN}" run --manifest-path "${ROOT_DIR}/crates/Cargo.toml" -p context_storage_ops -- "${SUBCOMMAND}" "$@"
fi

case "${SUBCOMMAND}" in
  ledger)
    exec python3 "${ROOT_DIR}/scripts/storage_ledger_capture.py" "$@"
    ;;
  weekly-lineage)
    exec python3 "${ROOT_DIR}/scripts/weekly_lineage_rollup.py" "$@"
    ;;
  cold-pack)
    exec python3 "${ROOT_DIR}/scripts/cold_snapshot_pack.py" "$@"
    ;;
  cold-tier)
    exec python3 "${ROOT_DIR}/scripts/cold_snapshot_tiering.py" "$@"
    ;;
  archive-ndjson)
    exec python3 "${ROOT_DIR}/scripts/archive_ndjson_by_time.py" "$@"
    ;;
  fanout-gc)
    exec python3 "${ROOT_DIR}/scripts/fanout_outbox_gc.py" "$@"
    ;;
  *)
    echo "ERROR: unsupported subcommand '${SUBCOMMAND}' and Rust runtime unavailable." >&2
    exit 2
    ;;
esac
