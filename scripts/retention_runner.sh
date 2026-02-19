#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# Load repo env so retention policy paths and thresholds are applied.
if [[ -f "$ROOT_DIR/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$ROOT_DIR/.env"
  set +a
fi

MAKE_BIN="${MAKE_BIN:-gmake}"
if ! command -v "$MAKE_BIN" >/dev/null 2>&1; then
  MAKE_BIN=make
fi

# High-cadence runs (<= 1h) should avoid per-run snapshot copies.
# If the caller already set QDRANT_SKIP_SNAPSHOT, keep that explicit value.
RETENTION_INTERVAL_SECONDS="${RETENTION_INTERVAL_SECONDS:-0}"
if [[ -z "${QDRANT_SKIP_SNAPSHOT:-}" ]]; then
  if [[ "$RETENTION_INTERVAL_SECONDS" =~ ^[0-9]+$ ]] && (( RETENTION_INTERVAL_SECONDS > 0 )) && (( RETENTION_INTERVAL_SECONDS <= 3600 )); then
    export QDRANT_SKIP_SNAPSHOT=1
    echo ">> qdrant snapshot disabled for high-cadence run (RETENTION_INTERVAL_SECONDS=${RETENTION_INTERVAL_SECONDS})"
  fi
fi

echo ">> telemetry archive"
"$MAKE_BIN" telemetry-archive

if [[ "${QDRANT_RETENTION_ENABLED:-1}" == "1" ]]; then
  echo ">> qdrant snapshot prune"
  "$MAKE_BIN" qdrant-snapshot-prune
else
  echo ">> qdrant snapshot prune (disabled)"
fi

if [[ "${MEMORY_BANK_EXPORT_ENABLED:-1}" == "1" ]]; then
  echo ">> memory bank export"
  if ! scripts/memory_bank_export.sh; then
    echo "!! memory bank export failed; continuing retention run" >&2
  fi
fi

if [[ "${MEMORY_BANK_PURGE_ENABLED:-1}" == "1" ]]; then
  echo ">> memory bank purge"
  if ! scripts/memory_bank_purge.sh; then
    echo "!! memory bank purge failed; continuing retention run" >&2
  fi
fi

if [[ "${FANOUT_OUTBOX_GC_ENABLED:-1}" == "1" ]]; then
  echo ">> fanout outbox gc"
  GC_ARGS=(
    --succeeded-retention-hours "${FANOUT_OUTBOX_SUCCEEDED_RETENTION_HOURS:-24}"
    --failed-retention-hours "${FANOUT_OUTBOX_FAILED_RETENTION_HOURS:-168}"
    --stale-pending-hours "${FANOUT_OUTBOX_STALE_PENDING_HOURS:-24}"
    --timeout-secs "${FANOUT_OUTBOX_GC_TIMEOUT_SECS:-15}"
  )
  if [[ -n "${FANOUT_OUTBOX_GC_DB_PATH:-}" ]]; then
    GC_ARGS+=(--db-path "${FANOUT_OUTBOX_GC_DB_PATH}")
  fi
  if [[ -n "${FANOUT_OUTBOX_STALE_TARGETS:-}" ]]; then
    GC_ARGS+=(--stale-targets "${FANOUT_OUTBOX_STALE_TARGETS}")
  fi
  if [[ "${FANOUT_OUTBOX_GC_VACUUM:-1}" == "1" ]]; then
    GC_ARGS+=(--vacuum --vacuum-min-deleted "${FANOUT_OUTBOX_GC_VACUUM_MIN_DELETED:-500}")
  fi
  if [[ "${FANOUT_OUTBOX_GC_DRY_RUN:-0}" == "1" ]]; then
    GC_ARGS+=(--dry-run)
  fi
  if ! python3 scripts/fanout_outbox_gc.py "${GC_ARGS[@]}"; then
    echo "!! fanout outbox gc failed; continuing retention run" >&2
  fi
fi
