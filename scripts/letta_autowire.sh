#!/usr/bin/env bash
set -euo pipefail

# --- Safe auth opts for set -u ---
declare -a AUTH_OPTS=()
if [[ -n "${LETTA_API_KEY:-}" ]]; then
  AUTH_OPTS=(-H "Authorization: Bearer ${LETTA_API_KEY}")
fi

# --- Config ---
LETTA_BASE="${LETTA_BASE:-http://127.0.0.1:8283/v1}"

# --- Safe auth opts for set -u ---
declare -a AUTH_OPTS=()
if [[ -n "${LETTA_API_KEY:-}" ]]; then
  AUTH_OPTS=(-H "Authorization: Bearer ${LETTA_API_KEY}")
fi

echo "Waiting for Letta at ${LETTA_BASE} ..."
for i in {1..60}; do
  if curl -fsS ${AUTH_OPTS+"${AUTH_OPTS+"${AUTH_OPTS[@]}"}"} "${LETTA_BASE}/health" >/dev/null 2>&1; then
    echo "   OK"
    break
  fi
  sleep 1
done

echo "==> (Optional) Wiring memory endpoints (Qdrant, MindsDB, MemoryBank) into Letta toolbelt"
# These endpoints are placeholders; keep || true so failures don't kill the seed
curl -fsS -X POST "${LETTA_BASE}/tools/mcp/register"       -H "Content-Type: application/json" ${AUTH_OPTS+"${AUTH_OPTS+"${AUTH_OPTS[@]}"}"}       -d '{"name":"qdrant","transport":"http","url":"http://127.0.0.1:53130/qdrant/mcp"}' >/dev/null 2>&1 || true

curl -fsS -X POST "${LETTA_BASE}/tools/mcp/register"       -H "Content-Type: application/json" ${AUTH_OPTS+"${AUTH_OPTS+"${AUTH_OPTS[@]}"}"}       -d '{"name":"memorymcp","transport":"http","url":"http://127.0.0.1:53130/memorymcp/mcp"}' >/dev/null 2>&1 || true

curl -fsS -X POST "${LETTA_BASE}/tools/mcp/register"       -H "Content-Type: application/json" ${AUTH_OPTS+"${AUTH_OPTS+"${AUTH_OPTS[@]}"}"}       -d '{"name":"mindsdb","transport":"http","url":"http://127.0.0.1:53130/mindsdb/mcp"}' >/dev/null 2>&1 || true

echo "==> Letta autowire completed"
