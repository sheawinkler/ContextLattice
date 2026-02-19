#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ORCH_URL="${MEMMCP_ORCHESTRATOR_URL:-${ORCH_BASE:-http://127.0.0.1:8075}}"
PROJECT="${PROJECT:-}"
FORCE_REQUEUE="${FORCE_REQUEUE:-false}"

RUN_MEMORY_BANK="${RUN_MEMORY_BANK:-true}"
RUN_QDRANT="${RUN_QDRANT:-true}"
RUN_MONGO_RAW="${RUN_MONGO_RAW:-true}"

MONGO_RAW_LIMIT="${MONGO_RAW_LIMIT:-120000}"
MONGO_RAW_TARGETS="${MONGO_RAW_TARGETS:-qdrant,mindsdb,letta}"
MONGO_RAW_MAX_PENDING_JOBS="${MONGO_RAW_MAX_PENDING_JOBS:-6000}"
MONGO_RAW_CHECK_INTERVAL="${MONGO_RAW_CHECK_INTERVAL:-200}"

MEMORY_BANK_LIMIT="${MEMORY_BANK_LIMIT:-5000}"
MEMORY_BANK_TARGETS="${MEMORY_BANK_TARGETS:-mongo_raw,qdrant,mindsdb,letta}"

QDRANT_LIMIT="${QDRANT_LIMIT:-50000}"
QDRANT_TARGETS="${QDRANT_TARGETS:-mongo_raw,mindsdb,letta}"
QDRANT_COLLECTION="${QDRANT_COLLECTION:-}"
QDRANT_INCLUDE_TARGET="${QDRANT_INCLUDE_TARGET:-false}"

WAIT_QUIET="${WAIT_QUIET:-true}"
QUIET_PENDING_MAX="${QUIET_PENDING_MAX:-800}"
QUIET_RETRYING_MAX="${QUIET_RETRYING_MAX:-500}"
QUIET_RUNNING_MAX="${QUIET_RUNNING_MAX:-120}"
QUIET_CONSECUTIVE="${QUIET_CONSECUTIVE:-3}"
POLL_SECS="${POLL_SECS:-20}"
MAX_WAIT_SECS="${MAX_WAIT_SECS:-7200}"
ORCH_API_KEY="${MEMMCP_ORCHESTRATOR_API_KEY:-}"

AUTH_HEADER=()
if [[ -n "$ORCH_API_KEY" ]]; then
  AUTH_HEADER=(-H "x-api-key: $ORCH_API_KEY")
fi

bool_is_true() {
  local v
  v="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')"
  [[ "$v" == "1" || "$v" == "true" || "$v" == "yes" || "$v" == "on" ]]
}

wait_for_quiet() {
  local phase="$1"
  if ! bool_is_true "$WAIT_QUIET"; then
    return 0
  fi

  local start_epoch now_epoch waited quiet_hits=0 telemetry pending retrying running at
  start_epoch="$(date +%s)"
  echo ">> [$phase] waiting for quiet fanout window on $ORCH_URL"
  while true; do
    now_epoch="$(date +%s)"
    waited="$((now_epoch - start_epoch))"
    if (( waited > MAX_WAIT_SECS )); then
      echo "!! [$phase] timed out waiting for quiet window after ${MAX_WAIT_SECS}s" >&2
      return 1
    fi
    telemetry="$(curl -fsS "$ORCH_URL/telemetry/fanout" "${AUTH_HEADER[@]}")"
    pending="$(printf '%s' "$telemetry" | jq -r '.summary.by_status.pending // 0')"
    retrying="$(printf '%s' "$telemetry" | jq -r '.summary.by_status.retrying // 0')"
    running="$(printf '%s' "$telemetry" | jq -r '.summary.by_status.running // 0')"
    at="$(printf '%s' "$telemetry" | jq -r '.updatedAt // empty')"
    if (( pending <= QUIET_PENDING_MAX && retrying <= QUIET_RETRYING_MAX && running <= QUIET_RUNNING_MAX )); then
      quiet_hits="$((quiet_hits + 1))"
    else
      quiet_hits=0
    fi
    echo ">> [$phase] poll at=${at} pending=${pending} retrying=${retrying} running=${running} quiet_hits=${quiet_hits}/${QUIET_CONSECUTIVE}"
    if (( quiet_hits >= QUIET_CONSECUTIVE )); then
      return 0
    fi
    sleep "$POLL_SECS"
  done
}

if bool_is_true "$RUN_MONGO_RAW"; then
  wait_for_quiet "mongo-raw"
  echo ">> running mongo-raw fanout backfill"
  payload="$(jq -n \
    --arg project "$PROJECT" \
    --argjson limit "$MONGO_RAW_LIMIT" \
    --arg targets "$MONGO_RAW_TARGETS" \
    --arg qdrant_collection "$QDRANT_COLLECTION" \
    --arg force_requeue "$FORCE_REQUEUE" \
    --argjson max_pending_jobs "$MONGO_RAW_MAX_PENDING_JOBS" \
    --argjson check_interval "$MONGO_RAW_CHECK_INTERVAL" '
    {
      project: (if $project == "" then null else $project end),
      limit: $limit,
      targets: (if $targets == "" then null else ($targets | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0))) end),
      qdrant_collection: (if $qdrant_collection == "" then null else $qdrant_collection end),
      force_requeue: ((["1","true","yes","on"] | index($force_requeue | ascii_downcase)) != null),
      max_pending_jobs: $max_pending_jobs,
      check_interval: $check_interval
    }'
  )"
  curl -fsS "$ORCH_URL/maintenance/fanout/backfill/mongo" \
    -H "content-type: application/json" \
    "${AUTH_HEADER[@]}" \
    -d "$payload" | jq
fi

if bool_is_true "$RUN_MEMORY_BANK"; then
  wait_for_quiet "memory-bank"
  echo ">> running memory-bank fanout rehydrate"
  MEMMCP_ORCHESTRATOR_URL="$ORCH_URL" \
  PROJECT="$PROJECT" \
  LIMIT="$MEMORY_BANK_LIMIT" \
  TARGETS="$MEMORY_BANK_TARGETS" \
  FORCE_REQUEUE="$FORCE_REQUEUE" \
  scripts/rehydrate_fanout.sh
fi

if bool_is_true "$RUN_QDRANT"; then
  wait_for_quiet "qdrant"
  echo ">> running qdrant-source fanout backfill"
  payload="$(jq -n \
    --arg project "$PROJECT" \
    --argjson limit "$QDRANT_LIMIT" \
    --arg targets "$QDRANT_TARGETS" \
    --arg qdrant_collection "$QDRANT_COLLECTION" \
    --arg force_requeue "$FORCE_REQUEUE" \
    --arg include_qdrant_target "$QDRANT_INCLUDE_TARGET" '
    {
      project: (if $project == "" then null else $project end),
      limit: $limit,
      targets: (if $targets == "" then null else ($targets | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0))) end),
      qdrant_collection: (if $qdrant_collection == "" then null else $qdrant_collection end),
      force_requeue: ((["1","true","yes","on"] | index($force_requeue | ascii_downcase)) != null),
      include_qdrant_target: ((["1","true","yes","on"] | index($include_qdrant_target | ascii_downcase)) != null)
    }'
  )"
  curl -fsS "$ORCH_URL/maintenance/fanout/backfill/qdrant" \
    -H "content-type: application/json" \
    "${AUTH_HEADER[@]}" \
    -d "$payload" | jq
fi

echo ">> done"
