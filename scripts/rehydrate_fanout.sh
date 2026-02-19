#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

ORCH_URL="${MEMMCP_ORCHESTRATOR_URL:-${ORCH_BASE:-http://127.0.0.1:8075}}"
PROJECT="${PROJECT:-}"
LIMIT="${LIMIT:-2000}"
TARGETS="${TARGETS:-}" # comma-separated; empty = orchestrator defaults
QDRANT_COLLECTION="${QDRANT_COLLECTION:-}"
FORCE_REQUEUE="${FORCE_REQUEUE:-0}"
ORCH_API_KEY="${MEMMCP_ORCHESTRATOR_API_KEY:-}"

FORCE_REQUEUE_NORMALIZED="$(printf '%s' "$FORCE_REQUEUE" | tr '[:upper:]' '[:lower:]')"
case "$FORCE_REQUEUE_NORMALIZED" in
  1|true|yes|on)
    FORCE_REQUEUE_BOOL=true
    ;;
  *)
    FORCE_REQUEUE_BOOL=false
    ;;
esac

AUTH_HEADER=()
if [[ -n "$ORCH_API_KEY" ]]; then
  AUTH_HEADER=(-H "x-api-key: $ORCH_API_KEY")
fi

payload=$(jq -n \
  --arg project "$PROJECT" \
  --argjson limit "$LIMIT" \
  --arg targets "$TARGETS" \
  --arg qdrant_collection "$QDRANT_COLLECTION" \
  --argjson force_requeue "$FORCE_REQUEUE_BOOL" '
  {
    project: (if $project == "" then null else $project end),
    limit: $limit,
    targets: (if $targets == "" then null else ($targets | split(",") | map(gsub("^\\s+|\\s+$"; "")) | map(select(length > 0)) ) end),
    qdrant_collection: (if $qdrant_collection == "" then null else $qdrant_collection end),
    force_requeue: $force_requeue
  }')

echo ">> rehydrate request: $payload"

curl -fsS "$ORCH_URL/maintenance/fanout/rehydrate" \
  -H "content-type: application/json" \
  "${AUTH_HEADER[@]}" \
  -d "$payload" | jq
