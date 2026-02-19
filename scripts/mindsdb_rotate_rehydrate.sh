#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ENV_FILE="${ENV_FILE:-.env}"
ORCH_URL="${MEMMCP_ORCHESTRATOR_URL:-${ORCH_BASE:-http://127.0.0.1:8075}}"
ORCH_API_KEY="${MEMMCP_ORCHESTRATOR_API_KEY:-}"
MINDSDB_SQL_URL="${MINDSDB_SQL_URL:-http://127.0.0.1:47334/api/sql/query}"

DB_NAME="${DB_NAME:-files_repair_$(date -u +%Y%m%d%H%M%S)}"
TABLE_NAME="${TABLE_NAME:-memory_events_repair_$(date -u +%Y%m%d%H%M%S)}"
SOURCE="${SOURCE:-both}" # mongo_raw | memory_bank | both
PROJECT="${PROJECT:-}"
MONGO_LIMIT="${MONGO_LIMIT:-150000}"
MEMORY_BANK_LIMIT="${MEMORY_BANK_LIMIT:-20000}"
MAX_PENDING_JOBS="${MAX_PENDING_JOBS:-6000}"
CHECK_INTERVAL="${CHECK_INTERVAL:-200}"
FORCE_REQUEUE="${FORCE_REQUEUE:-false}"
RESTART_SERVICES="${RESTART_SERVICES:-true}"
QUIET_OUTSTANDING_MAX="${QUIET_OUTSTANDING_MAX:-5000}"
QUIET_CONSECUTIVE="${QUIET_CONSECUTIVE:-2}"
POLL_SECS="${POLL_SECS:-15}"
MAX_WAIT_SECS="${MAX_WAIT_SECS:-1800}"

usage() {
  cat <<'USAGE'
Usage: scripts/mindsdb_rotate_rehydrate.sh [options]

Options:
  --db <name>               MindsDB autosync database target (default: files_repair_<timestamp>)
  --table <name>            New MindsDB autosync table (default: memory_events_repair_<timestamp>)
  --source <mode>           mongo_raw | memory_bank | both (default: both)
  --project <name>          Optional single project scope
  --mongo-limit <n>         Mongo rows to scan for mongo_raw backfill
  --memory-limit <n>        Files to scan for memory-bank rehydrate
  --max-pending <n>         Backfill throttle threshold for outstanding fanout jobs
  --check-interval <n>      Queue check interval during mongo scan
  --force-requeue           Force requeue already-succeeded fanout jobs
  --skip-restart            Do not restart services after env changes
  --orchestrator-url <url>  Override orchestrator URL
USAGE
}

bool_is_true() {
  local v
  v="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')"
  [[ "$v" == "1" || "$v" == "true" || "$v" == "yes" || "$v" == "on" ]]
}

mindsdb_sql() {
  local query="$1"
  curl -fsS -X POST "$MINDSDB_SQL_URL" \
    -H 'content-type: application/json' \
    -d "$(jq -n --arg query "$query" '{query:$query}')"
}

db_supports_create_table() {
  local db="$1"
  local probe_table
  local create_resp
  local error_msg
  probe_table="__memmcp_probe_$(date +%s)"
  mindsdb_sql "CREATE DATABASE IF NOT EXISTS ${db};" >/dev/null || true
  create_resp="$(mindsdb_sql "CREATE TABLE IF NOT EXISTS ${db}.${probe_table} (id INT);")"
  error_msg="$(printf '%s' "$create_resp" | jq -r '.error_message // empty' 2>/dev/null || true)"
  if [[ -n "$error_msg" ]]; then
    if [[ "$error_msg" == *"Can't create table"* ]]; then
      return 1
    fi
    return 1
  fi
  mindsdb_sql "DROP TABLE ${db}.${probe_table};" >/dev/null || true
  return 0
}

set_env_key() {
  local key="$1"
  local value="$2"
  local tmp_file
  tmp_file="$(mktemp "${ENV_FILE}.tmp.XXXXXX")"
  if [[ -f "$ENV_FILE" ]]; then
    awk -v k="$key" -v v="$value" '
      BEGIN { updated = 0 }
      $0 ~ ("^" k "=") {
        if (!updated) {
          print k "=" v
          updated = 1
        }
        next
      }
      { print }
      END {
        if (!updated) {
          print k "=" v
        }
      }
    ' "$ENV_FILE" > "$tmp_file"
  else
    printf '%s=%s\n' "$key" "$value" > "$tmp_file"
  fi
  mv "$tmp_file" "$ENV_FILE"
}

wait_for_quiet() {
  local start quiet_hits telemetry pending retrying running outstanding now waited
  start="$(date +%s)"
  quiet_hits=0
  while true; do
    telemetry="$(curl -fsS "$ORCH_URL/telemetry/fanout" "${AUTH_HEADER[@]}")"
    pending="$(printf '%s' "$telemetry" | jq -r '.summary.by_status.pending // 0')"
    retrying="$(printf '%s' "$telemetry" | jq -r '.summary.by_status.retrying // 0')"
    running="$(printf '%s' "$telemetry" | jq -r '.summary.by_status.running // 0')"
    outstanding="$((pending + retrying + running))"
    if (( outstanding <= QUIET_OUTSTANDING_MAX )); then
      quiet_hits="$((quiet_hits + 1))"
    else
      quiet_hits=0
    fi
    if (( quiet_hits >= QUIET_CONSECUTIVE )); then
      break
    fi
    now="$(date +%s)"
    waited="$((now - start))"
    if (( waited > MAX_WAIT_SECS )); then
      echo "!! timed out waiting for quiet fanout window" >&2
      return 1
    fi
    sleep "$POLL_SECS"
  done
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --db)
      [[ $# -ge 2 ]] || { echo "Missing value for --db" >&2; exit 2; }
      DB_NAME="$2"
      shift 2
      ;;
    --table)
      [[ $# -ge 2 ]] || { echo "Missing value for --table" >&2; exit 2; }
      TABLE_NAME="$2"
      shift 2
      ;;
    --source)
      [[ $# -ge 2 ]] || { echo "Missing value for --source" >&2; exit 2; }
      SOURCE="$2"
      shift 2
      ;;
    --project)
      [[ $# -ge 2 ]] || { echo "Missing value for --project" >&2; exit 2; }
      PROJECT="$2"
      shift 2
      ;;
    --mongo-limit)
      [[ $# -ge 2 ]] || { echo "Missing value for --mongo-limit" >&2; exit 2; }
      MONGO_LIMIT="$2"
      shift 2
      ;;
    --memory-limit)
      [[ $# -ge 2 ]] || { echo "Missing value for --memory-limit" >&2; exit 2; }
      MEMORY_BANK_LIMIT="$2"
      shift 2
      ;;
    --max-pending)
      [[ $# -ge 2 ]] || { echo "Missing value for --max-pending" >&2; exit 2; }
      MAX_PENDING_JOBS="$2"
      shift 2
      ;;
    --check-interval)
      [[ $# -ge 2 ]] || { echo "Missing value for --check-interval" >&2; exit 2; }
      CHECK_INTERVAL="$2"
      shift 2
      ;;
    --force-requeue)
      FORCE_REQUEUE=true
      shift
      ;;
    --skip-restart)
      RESTART_SERVICES=false
      shift
      ;;
    --orchestrator-url)
      [[ $# -ge 2 ]] || { echo "Missing value for --orchestrator-url" >&2; exit 2; }
      ORCH_URL="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

case "$SOURCE" in
  mongo_raw|memory_bank|both) ;;
  *)
    echo "Invalid --source value: $SOURCE" >&2
    exit 2
    ;;
esac

AUTH_HEADER=()
if [[ -n "$ORCH_API_KEY" ]]; then
  AUTH_HEADER=(-H "x-api-key: $ORCH_API_KEY")
fi

echo ">> rotating MindsDB autosync target to ${DB_NAME}.${TABLE_NAME}"
if [[ "$DB_NAME" != "files" ]]; then
  if ! db_supports_create_table "$DB_NAME"; then
    echo ">> warning: ${DB_NAME} does not support CREATE TABLE on this MindsDB build; falling back to files database"
    DB_NAME="files"
    if [[ "$TABLE_NAME" == memory_events* ]]; then
      TABLE_NAME="memory_events_repair_$(date -u +%Y%m%d%H%M%S)"
    fi
  fi
fi
set_env_key "MINDSDB_AUTOSYNC_DB" "$DB_NAME"
set_env_key "MINDSDB_AUTOSYNC_TABLE" "$TABLE_NAME"

if bool_is_true "$RESTART_SERVICES"; then
  echo ">> restarting mindsdb + proxy + orchestrator"
  docker compose up -d mindsdb mindsdb-http-proxy >/dev/null
  docker compose build memmcp-orchestrator >/dev/null
  docker compose up -d memmcp-orchestrator >/dev/null
fi

for i in {1..90}; do
  if curl -fsS "$ORCH_URL/health" "${AUTH_HEADER[@]}" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

if [[ "$SOURCE" == "mongo_raw" || "$SOURCE" == "both" ]]; then
  echo ">> running mongo_raw -> mindsdb controlled backfill"
  payload="$(jq -n \
    --arg project "$PROJECT" \
    --argjson limit "$MONGO_LIMIT" \
    --arg force_requeue "$FORCE_REQUEUE" \
    --argjson max_pending_jobs "$MAX_PENDING_JOBS" \
    --argjson check_interval "$CHECK_INTERVAL" '
    {
      project: (if $project == "" then null else $project end),
      limit: $limit,
      targets: ["mindsdb"],
      force_requeue: ((["1","true","yes","on"] | index($force_requeue | ascii_downcase)) != null),
      max_pending_jobs: $max_pending_jobs,
      check_interval: $check_interval
    }')"
  curl -fsS "$ORCH_URL/maintenance/fanout/backfill/mongo" \
    -H 'content-type: application/json' \
    "${AUTH_HEADER[@]}" \
    -d "$payload" | jq
fi

if [[ "$SOURCE" == "memory_bank" || "$SOURCE" == "both" ]]; then
  wait_for_quiet
  echo ">> running memory-bank -> mindsdb rehydrate"
  payload="$(jq -n \
    --arg project "$PROJECT" \
    --argjson limit "$MEMORY_BANK_LIMIT" \
    --arg force_requeue "$FORCE_REQUEUE" '
    {
      project: (if $project == "" then null else $project end),
      limit: $limit,
      targets: ["mindsdb"],
      force_requeue: ((["1","true","yes","on"] | index($force_requeue | ascii_downcase)) != null)
    }')"
  curl -fsS "$ORCH_URL/maintenance/fanout/rehydrate" \
    -H 'content-type: application/json' \
    "${AUTH_HEADER[@]}" \
    -d "$payload" | jq
fi

echo ">> done"
