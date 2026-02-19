# Client Integration Guide

Use the MCP hub (`http://127.0.0.1:53130/<server>/mcp`) as the single host for every IDE agent or CLI. It routes to Memory Bank, Qdrant, and optionally MindsDB via the server key (e.g., `memorymcp`, `qdrant`, `mindsdb`).

### Fast install

Run `scripts/install_mcp_clients.sh` to copy the templates in `configs/` to all default locations (Windsurf, Cline, Cursor, Claude). Set `WINDSURF_CONF`, `CLINE_CONF`, etc., before running if you keep configs elsewhere. Restart each IDE afterward.

## Shared MCP settings

```
Base URL (memory bank): http://127.0.0.1:53130/memorymcp/mcp
Transport: streamable-http (accept: application/json, text/event-stream)
Headers:
  MCP-Protocol-Version: 2025-11-25
  MCP-Transport: streamable-http
```

If a client does not expose raw headers, point it to the orchestrator HTTP shim instead: `http://127.0.0.1:8075` (see the Trae example below).

## Orchestrator auth (optional)

If `MEMMCP_ORCHESTRATOR_API_KEY` is set, include one of these headers in REST calls:

- `x-api-key: <key>`
- `authorization: Bearer <key>`

## Terminal dashboard

```bash
python3 scripts/terminal_dashboard.py --interval 5
```

## Pathing conventions (orchestrator REST)

- `projectName` is a single slug — **do not** include `/` or `\` (use `mem_mcp_lobehub`, not `mem/mcp`).
- `fileName` may include `/` for directory-style organization (keep it relative; no leading `/`).
- When calling `GET /memory/files/{project}/{file}`, URL-encode each path segment (not the full path) so slashes remain separators.

Example (file name contains nested folders + a colon):
```
curl -fsS "http://127.0.0.1:8075/memory/files/mem_mcp_lobehub/decisions/2026-01-27/solana%3Anote.txt"
```
If a segment contains spaces or special chars, encode that segment only:
```
curl -fsS "http://127.0.0.1:8075/memory/files/mem_mcp_lobehub/decisions/2026-01-27/solana%20note.txt"
```

## Missing file behavior (orchestrator REST)

- `GET /memory/files/{project}/{file}` now treats missing files as recoverable.
- For `index__*.json` files, the orchestrator auto-creates a bootstrap stub that points to the expected `__latest` file.
- For `sol_scaler_overrides/override-smoke-test.json`, the orchestrator auto-creates a placeholder smoke-test record.
- Toggle with `ORCH_MISSING_FILE_AUTOSTUB=true|false` (default: `true`).

## Federated retrieval via orchestrator

`POST /memory/search` now runs a merged retrieval pass across `qdrant`, `mongo_raw`,
`mindsdb`, `letta`, and `memory_bank` lexical fallback.

Example:
```bash
curl -fsS http://127.0.0.1:8075/memory/search \
  -H 'content-type: application/json' \
  -d '{
    "query": "rpc provider fallback policy",
    "project": "algotraderv2_rust",
    "limit": 8,
    "include_retrieval_debug": true
  }' | jq
```

Optional controls:
- `sources`: constrain specific backends (`["qdrant","mongo_raw"]`)
- `source_weights`: ranking bias per source
- `rerank_with_learning`: apply preference-based reranking
- `fetch_content`: fetch full memory-bank file content for each hit

### Other MCP hub endpoints

- Qdrant: `http://127.0.0.1:53130/qdrant/mcp`
- MindsDB: `http://127.0.0.1:53130/mindsdb/mcp` (only when `MCP_HUB_CONFIG` uses `configs/mcp-hub.config.json`; not present in lite config)

## Windsurf

1. Open `~/.codeium/windsurf/mcp_config.json` (path from `.env:WINDSURF_CONF`).
2. Add an entry:
   ```json
   {
     "name": "memmcp",
     "transport": "streamable-http",
     "url": "http://127.0.0.1:53130/memorymcp/mcp"
   }
   ```
3. Restart Windsurf or reload MCP servers.

## Cline (VS Code)

1. Open the `cline.mcp.json` (command palette → “Cline: Open MCP Config”).
2. Append:
   ```json
   {
     "name": "memmcp",
     "type": "http",
     "serverUrl": "http://127.0.0.1:53130/memorymcp/mcp"
   }
   ```
3. Save and run `Cline: Reload MCP Servers`.

## Cursor IDE

1. Edit `~/Library/Application Support/Cursor/User/globalStorage/mcp-servers.json` (or use the MCP UI panel).
2. Add the same HTTP entry as above. Cursor v0.42+ autodetects streamable-http and will reuse the single endpoint for every workspace.

## Claude Code / Claude Desktop

Claude’s MCP beta looks for `~/.mcp/servers.json`:
```json
{
  "servers": [
    {
      "name": "memmcp",
      "type": "http",
      "serverUrl": "http://127.0.0.1:53130/memorymcp/mcp"
    }
  ]
}
```
Restart Claude Code and verify the “memmcp” toolset appears in the MCP panel.

## Trae Agent + Ollama (lightweight)

1. Edit `~/.trae_agent/trae_config.yaml` (or copy from `trae_config.template.yaml`).
2. Set the default client to the local Ollama shim via the `ollama_openai` provider:
   ```yaml
   model_providers:
     ollama_openai:
       provider: openai
       api_key: local-demo
       base_url: http://127.0.0.1:11434/v1
     lmstudio:
       provider: openai
       api_key: local
       base_url: http://localhost:1234/v1

   models:
     qwen_small:
       model_provider: ollama_openai
       model: llama3.2:1b
   ```
  - Use `llama3.2:1b` or any other model you have pulled (`ollama pull llama3.2:1b`).

## Task agents (optional)
For task orchestration and local agent runners (Trae, AutoGen, CrewAI, LangGraph, OpenHands), see `docs/task_agents.md`.
   - Increase `max_steps` in the task block if Trae needs more thinking time.
3. Point the MCP target at the hub:
   ```yaml
   mcp_servers:
     memory:
       transport: http
       url: http://127.0.0.1:53130/memorymcp/mcp
   ```
4. To switch back to LM Studio or a hosted OpenAI-compatible API, change the `base_url` under `model_providers.lmstudio` (or add another provider) and point the desired model at it.
5. Run `trae-cli run --config ~/.trae_agent/trae_config.yaml`.

## Self-test checklist

- `curl -fsS http://127.0.0.1:8075/status | jq` → memory-bank + qdrant healthy.
- `curl -fsS http://127.0.0.1:53130/memorymcp/mcp` (with MCP headers) → shows memory-bank toolset.
- From each IDE, run a “list projects” tool and ensure results match `~/Documents/Projects` memories.

Keep this file updated as we bring more clients online.
