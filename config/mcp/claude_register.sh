#!/usr/bin/env bash
set -euo pipefail
claude mcp add --transport http hub-http http://127.0.0.1:53130/memorymcp/mcp || true
claude mcp add --transport sse  hub-sse  http://127.0.0.1:53130/memorymcp/sse || true
echo "Registered MCP Hub for Claude."
