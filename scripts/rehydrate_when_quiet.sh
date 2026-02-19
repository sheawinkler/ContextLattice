#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ORCH_URL="${MEMMCP_ORCHESTRATOR_URL:-${ORCH_BASE:-http://127.0.0.1:8075}}"
PROJECT="${PROJECT:-}"
LIMIT="${LIMIT:-2000}"
TARGETS="${TARGETS:-mongo_raw,qdrant,mindsdb,letta}"
FORCE_REQUEUE="${FORCE_REQUEUE:-false}"

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

start_epoch="$(date +%s)"
quiet_hits=0

echo ">> waiting for quiet fanout window on $ORCH_URL"
echo ">> thresholds: pending<=$QUIET_PENDING_MAX retrying<=$QUIET_RETRYING_MAX running<=$QUIET_RUNNING_MAX for $QUIET_CONSECUTIVE consecutive polls"

while true; do
  now_epoch="$(date +%s)"
  waited="$((now_epoch - start_epoch))"
  if (( waited > MAX_WAIT_SECS )); then
    echo "!! timed out waiting for quiet window after ${MAX_WAIT_SECS}s" >&2
    exit 1
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

  echo ">> poll at=${at} pending=${pending} retrying=${retrying} running=${running} quiet_hits=${quiet_hits}/${QUIET_CONSECUTIVE}"

  if (( quiet_hits >= QUIET_CONSECUTIVE )); then
    break
  fi
  sleep "$POLL_SECS"
done

echo ">> quiet window reached; triggering rehydrate"
MEMMCP_ORCHESTRATOR_URL="$ORCH_URL" \
PROJECT="$PROJECT" \
LIMIT="$LIMIT" \
TARGETS="$TARGETS" \
FORCE_REQUEUE="$FORCE_REQUEUE" \
scripts/rehydrate_fanout.sh
