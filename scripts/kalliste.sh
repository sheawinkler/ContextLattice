#!/usr/bin/env bash
set -euo pipefail

MCP_PROXY_PORT="${MCP_PROXY_PORT:-9092}"

echo ">> writing configs/mcp-proxy.config.json"
cat > configs/mcp-proxy.config.json <<JSON
{
  "mcpProxy": {
    "type": "http",
    "addr": ":${MCP_PROXY_PORT}",
    "baseURL": "http://localhost:${MCP_PROXY_PORT}"
  },
  "clients": {
    "qdrant": {
      "type": "http",
      "serverUrl": "http://mcp-qdrant:8000/mcp"
    },
    "mindsdb": {
      "type": "http",
      "serverUrl": "http://mindsdb-http-proxy:8004/mcp"
    },
    "memorymcp": {
      "type": "http",
      "serverUrl": "http://memorymcp-http:59081/mcp"
    }
  }
}
JSON

echo ">> writing docker-compose.override.yml (adds mcp-proxy service)"
cat > docker-compose.override.yml <<YML
services:
  mcp-proxy:
    image: ghcr.io/tbxark/mcp-proxy:v0.39.1
    restart: unless-stopped
    ports:
      - "${MCP_PROXY_PORT}:${MCP_PROXY_PORT}"
    volumes:
      - ./configs/mcp-proxy.config.json:/config/config.json:ro
    command: ["--config", "/config/config.json"]
YML

echo ">> bringing up mcp-proxy ..."
docker compose up -d --remove-orphans mcp-proxy

echo ">> probing proxy status on :${MCP_PROXY_PORT}"
if ! curl -fsS "http://localhost:${MCP_PROXY_PORT}/status" >/dev/null 2>&1; then
  echo "WARN: proxy status endpoint unavailable; continuing"
fi

echo ">> All set. Point IDE/agents at: http://localhost:${MCP_PROXY_PORT}/servers/<name>/mcp"
