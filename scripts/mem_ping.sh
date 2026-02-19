#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -f "${ROOT_DIR}/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "${ROOT_DIR}/.env"
  set +a
fi

MCP_PORT="${MCP_HUB_PORT:-53130}"
MCP_VERSION="${MCP_PROTOCOL_VERSION:-2025-11-25}"
BASE_URL="http://127.0.0.1:${MCP_PORT}"

declare -a CANDIDATE_URLS
if [[ -n "${MCP_HUB_URL:-}" ]]; then
  CANDIDATE_URLS=(
    "${MCP_HUB_URL}"
    "${BASE_URL}/memorymcp/mcp"
    "${BASE_URL}/memorymcp/"
    "${BASE_URL}/memorymcp"
  )
else
  CANDIDATE_URLS=(
    "${BASE_URL}/memorymcp/mcp"
    "${BASE_URL}/memorymcp/"
    "${BASE_URL}/memorymcp"
  )
fi

MCP_URL=""
RESP=""
ATTEMPTS="${MCP_PING_ATTEMPTS:-45}"
SLEEP_SECONDS="${MCP_PING_SLEEP_SECONDS:-1}"

for _ in $(seq 1 "${ATTEMPTS}"); do
  for candidate in "${CANDIDATE_URLS[@]}"; do
    if RESP="$(curl -fsS \
      -H 'Content-Type: application/json' \
      -H 'Accept: application/json, text/event-stream' \
      -H "MCP-Protocol-Version: ${MCP_VERSION}" \
      -d '{"jsonrpc":"2.0","id":"mem-ping","method":"tools/list","params":{}}' \
      "${candidate}" 2>/dev/null)"; then
      MCP_URL="${candidate}"
      break 2
    fi
  done
  sleep "${SLEEP_SECONDS}"
done

if [[ -z "${MCP_URL}" ]]; then
  echo "MCP tools/list probe failed for candidates:" >&2
  for candidate in "${CANDIDATE_URLS[@]}"; do
    echo "  - ${candidate}" >&2
  done
  exit 1
fi

echo "== MCP tools/list: ${MCP_URL} (v${MCP_VERSION}) =="

if command -v jq >/dev/null 2>&1; then
  echo "${RESP}" | jq
else
  echo "${RESP}"
fi
