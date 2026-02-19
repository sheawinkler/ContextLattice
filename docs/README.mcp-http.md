# MCP (HTTP-only) quickstart

This project exposes a Model Context Protocol server over **Streamable HTTP**.
**Send each JSON-RPC message as an HTTP POST** to the MCP endpoint, and include:

- `Accept: application/json, text/event-stream`
- `Content-Type: application/json`
- `MCP-Protocol-Version: 2025-11-25`

**Endpoint (memory bank via hub):** `http://127.0.0.1:53130/memorymcp/mcp`

Examples: curl -fsS http://127.0.0.1:53130/memorymcp/mcp \
  -H 'accept: application/json, text/event-stream' \
  -H 'content-type: application/json' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"kalliste-alpha","version":"dev"}}}'

# list tools
curl -fsS http://127.0.0.1:53130/memorymcp/mcp \
  -H 'accept: application/json, text/event-stream' \
  -H 'content-type: application/json' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{"jsonrpc":"2.0","id":"tools-1","method":"tools/list","params":{}}'
Notes:
	•	MCP spec (v2025-11-25) requires POST to a single MCP endpoint, with Accept listing both JSON and event-stream.  
	•	mcp-proxy routes by server key: `/memorymcp/mcp`, `/qdrant/mcp` (and `/mindsdb/mcp` when full hub config is enabled).  
	•	Supergateway’s Streamable HTTP path defaults to /mcp but is configurable.  
