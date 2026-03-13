#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-$ROOT_DIR/.env}"
DRY_RUN="${DRY_RUN:-0}"
FORCE_NON_EMPTY="${FORCE_NON_EMPTY:-0}"

if [[ -f "$ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
fi

PROJECT="${COMPOSE_PROJECT_NAME:-$(basename "$ROOT_DIR")}"
STAMP="$(date +%Y%m%d_%H%M%S)"
LOG_DIR="$ROOT_DIR/logs"
REPORT_PATH="$LOG_DIR/hot-storage-migration-$STAMP.log"
mkdir -p "$LOG_DIR"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required" >&2
  exit 2
fi

copy_to_named_volume() {
  local key="$1"
  local src="$2"
  local volume_key="$3"
  local volume_name="${PROJECT}_${volume_key}"

  echo "[$key] source=$src target_volume=$volume_name" | tee -a "$REPORT_PATH"

  if [[ ! -d "$src" ]]; then
    echo "[$key] source path missing, skipping" | tee -a "$REPORT_PATH"
    return 0
  fi

  if [[ "$DRY_RUN" == "1" ]]; then
    echo "[$key] DRY_RUN=1, no copy performed" | tee -a "$REPORT_PATH"
    return 0
  fi

  docker volume create "$volume_name" >/dev/null

  local entries
  entries="$(docker run --rm -v "${volume_name}:/to" alpine:3.20 sh -c 'find /to -mindepth 1 -maxdepth 1 | wc -l' | tr -d '[:space:]')"
  if [[ "${entries:-0}" != "0" && "$FORCE_NON_EMPTY" != "1" ]]; then
    echo "[$key] target volume already has data; skipping (set FORCE_NON_EMPTY=1 to merge)" | tee -a "$REPORT_PATH"
    return 0
  fi

  docker run --rm \
    -v "${src}:/from:ro" \
    -v "${volume_name}:/to" \
    alpine:3.20 sh -ceu '
      if [ -d /from ]; then
        cp -a /from/. /to/
      fi
    '

  echo "[$key] copy complete" | tee -a "$REPORT_PATH"
}

migrate_if_bind_mount() {
  local key="$1"
  local value="$2"
  local volume_key="$3"

  if [[ -z "$value" ]]; then
    echo "[$key] unset, skipping" | tee -a "$REPORT_PATH"
    return 0
  fi

  if [[ "$value" != /* ]]; then
    echo "[$key] already non-bind value ($value), skipping" | tee -a "$REPORT_PATH"
    return 0
  fi

  copy_to_named_volume "$key" "$value" "$volume_key"
}

echo "project=$PROJECT dry_run=$DRY_RUN force_non_empty=$FORCE_NON_EMPTY" | tee -a "$REPORT_PATH"

migrate_if_bind_mount "QDRANT_STORAGE" "${QDRANT_STORAGE:-}" "qdrant_storage"
migrate_if_bind_mount "MONGO_DATA" "${MONGO_DATA:-}" "mongo_data"
migrate_if_bind_mount "MEMORY_BANK_DATA" "${MEMORY_BANK_DATA:-}" "memory_bank_data"
migrate_if_bind_mount "MINDSDB_DATA" "${MINDSDB_DATA:-}" "mindsdb_data"
migrate_if_bind_mount "LETTA_PGDATA" "${LETTA_PGDATA:-}" "letta_pg"
migrate_if_bind_mount "LF_PGDATA" "${LF_PGDATA:-}" "lf_pgdata"
migrate_if_bind_mount "CLICKHOUSE_DATA" "${CLICKHOUSE_DATA:-}" "clickhouse_data"
migrate_if_bind_mount "LF_MINIO_DATA" "${LF_MINIO_DATA:-}" "lf_minio_data"

echo "done report=$REPORT_PATH" | tee -a "$REPORT_PATH"
