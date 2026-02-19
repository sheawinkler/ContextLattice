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
MCP_URL="${MCP_HUB_URL:-http://127.0.0.1:${MCP_PORT}/memorymcp/mcp}"
MCP_VERSION="${MCP_PROTOCOL_VERSION:-2025-11-25}"

echo "== MCP tools/list: ${MCP_URL} (v${MCP_VERSION}) =="

RESP="$(curl -fsS \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "MCP-Protocol-Version: ${MCP_VERSION}" \
  -d '{"jsonrpc":"2.0","id":"mem-ping","method":"tools/list","params":{}}' \
  "${MCP_URL}")"

if command -v jq >/dev/null 2>&1; then
  echo "${RESP}" | jq
else
  echo "${RESP}"
fi
