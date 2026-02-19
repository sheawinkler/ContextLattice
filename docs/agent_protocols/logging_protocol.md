# Agent Context: memMCP Logging + Recall (Updated 2026-02-08)

## Why this matters
- memMCP is the shared memory stack for every agent working on these machines—not just algotraderv2.
- Log **every meaningful decision, assumption, and configuration change** so the next agent can pick up instantly, regardless of project.
- Fallback to these instructions anytime you forget how to record or retrieve context.

## Two entrypoints
1. **Orchestrator shim (easy mode)** – `http://127.0.0.1:8075`
   - Provides REST helpers such as `/memory/write`, `/memory/files/{project}/{file}`, `/telemetry/*`.
   - Ideal for quick decision logging and fetching existing notes.
2. **Raw MCP hub (advanced JSON-RPC)** – `http://127.0.0.1:53130/memorymcp/mcp`
   - Use when you need direct access to `tools/list`, `list_project_files`, `memory_bank_read`, `memory_bank_write`, `memory_bank_update`.
   - Requires headers: `Accept: application/json, text/event-stream`, `Content-Type: application/json`, `MCP-Protocol-Version: 2025-11-25`.
   - `search_memory` is not part of the current memory-bank MCP toolset.

## Default environment variables
```
export MEMMCP_ORCHESTRATOR_URL=${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}
export MEMMCP_PROJECT=${MEMMCP_PROJECT:-algotraderv2_rust}
```
Override them per run if you are logging for a different project (e.g., `MEMMCP_PROJECT=mem_mcp_lobehub`).

## Logging a decision (use this every time)
```
curl -fsS "$MEMMCP_ORCHESTRATOR_URL/memory/write" \
  -H 'content-type: application/json' \
  -d '{
        "projectName": "algotraderv2_rust",
        "fileName": "decisions/20251212_whatever.txt",
        "content": "<short paragraph about what changed and why>"
      }'
```
Guidelines:
- File names should sort chronologically: `decisions/YYYYMMDD_context.txt`.
- Summaries must mention the exact files or subsystems touched plus current status (e.g., tests run, runs pending).
- If logging multiple items at once, append them in the same `content` string separated by `\n`.

## Pathing conventions (REST)
- `projectName` must be a single slug (no `/` or `\`). Use `mem_mcp_lobehub`, not `mem/mcp`.
- `fileName` can include `/` to create folders. Keep it relative (no leading `/`).
- When reading via `GET /memory/files/{project}/{file}`, URL-encode each path segment (not the whole path) so slashes stay as separators.

Example read (folder + special character in filename):
```
curl -fsS "$MEMMCP_ORCHESTRATOR_URL/memory/files/mem_mcp_lobehub/decisions/2026-01-27/solana%3Anote.txt"
```

## Retrieving prior context quickly
- REST read: `curl -fsS "$MEMMCP_ORCHESTRATOR_URL/memory/files/<project>/<path>` (swap `<project>` for whatever you used in `projectName`).
- JSON-RPC list files in a project:
```
curl -fsS http://127.0.0.1:53130/memorymcp/mcp \
  -H 'accept: application/json, text/event-stream' \
  -H 'content-type: application/json' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{
        "jsonrpc":"2.0","id":"list-1","method":"tools/call",
        "params":{"name":"list_project_files","arguments":{"projectName":"algotraderv2_rust"}}
      }'
```
- JSON-RPC read a specific file:
```
curl -fsS http://127.0.0.1:53130/memorymcp/mcp \
  -H 'accept: application/json, text/event-stream' \
  -H 'content-type: application/json' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{
        "jsonrpc":"2.0","id":"read-1","method":"tools/call",
        "params":{"name":"memory_bank_read","arguments":{"projectName":"algotraderv2_rust","fileName":"decisions/20251212_whatever.txt"}}
      }'
```
- Semantic recall lives under Qdrant tools, not memory-bank tools:
```
curl -fsS http://127.0.0.1:53130/qdrant/mcp \
  -H 'accept: application/json, text/event-stream' \
  -H 'content-type: application/json' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{
        "jsonrpc":"2.0","id":"qdrant-1","method":"tools/call",
        "params":{"name":"qdrant-find","arguments":{"collection_name":"memmcp_notes","query":"helius"}}
      }'
```
- Need a directory listing? Use `/memory/files/{project}/decisions/index.json` if you created one, or list via the JSON-RPC `list_project_files` tool.

## Health checks
```
curl -fsS http://127.0.0.1:8075/status | jq    # orchestrator + memory bank state
curl -fsS http://127.0.0.1:8075/telemetry/trading | jq   # latest trading snapshot pushed from algotrader
```
If `/memory/write` fails, queue your note locally and retry—**never skip logging**.

## Expectations for all agents (any repo)
- Record every outstanding TODO, test run, production change, or investigative note—no exceptions.
- When you finish a session, append a “handoff” file under `briefings/` summarizing repo state and remaining blockers for that project.
- Before editing, search memMCP for the subsystem you are touching to avoid duplicating work.
- If you add a new workflow, automation, or convention, update this instruction file (in every location) and log the change.
