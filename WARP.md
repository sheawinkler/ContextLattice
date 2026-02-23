# WARP.md

This file provides guidance to WARP (warp.dev) when working with code in this repository.

## Architecture Overview

**kalliste-alpha** is a local-first memory service for AI agents that standardizes on HTTP-only Model Context Protocol (MCP). The system is designed to run entirely via Docker Compose with a single-command launch.

### Core Services (layered architecture)

1. **Storage Layer**
   - **Qdrant** (`:6333` internal): Vector database for memory embeddings. Not published to host by default; access via internal DNS `http://qdrant:6333`.
   - **MongoDB** (`:27017` internal): Document store for memory bank persistence.

2. **MCP Providers**
   - **mcp-qdrant** (`:8000` internal): HTTP-wrapped Qdrant MCP server
   - **memorymcp-http** (`:59081`): Memory bank MCP server via npx supergateway, backed by MongoDB
   - **mindsdb-http-proxy** (`:8004`): FastMCP proxy for MindsDB SSE → HTTP

3. **Gateway Layer**
   - **mcp-hub** (`:53130`): MCP router (TBXark mcp-proxy). Access servers at `/<server>/mcp` (always `/memorymcp/mcp`, `/qdrant/mcp`; `/mindsdb/mcp` only when full hub config is enabled)
   - **memmcp-orchestrator** (`:8075`): REST convenience layer (`/memory/write`, `/memory/files/{project}/{file}`, `/status`, `/telemetry/*`)

4. **Optional Services (profiles)**
   - **llm**: Ollama (`:11434`), Letta (`:8283`)
   - **analytics**: MindsDB (`:47334`, `:47337`)
   - **observability**: Langfuse (`:15510`) + ClickHouse + Postgres, Promptfoo (`:15500`)

### Data Flow

```
AI Agent → Orchestrator (:8075) OR mcp-hub (:53130/<server>/mcp) 
       → memorymcp-http / mcp-qdrant / mindsdb-http-proxy
       → MongoDB / Qdrant / MindsDB
```

**Decision logging pattern**: Agents call `POST :8075/memory/write` with `{projectName, fileName, content}` to persist context. Files are organized as `decisions/YYYYMMDD_*.txt` or `briefings/` for handoffs.

### Current Interface Contracts (2026-02-10)

- **Orchestrator write payload is strict**: `/memory/write` requires `projectName`, `fileName`, and `content` (optional `topicPath`). A payload with only `content` returns `422`.
- **Memory-bank MCP tools are limited to**: `list_projects`, `list_project_files`, `memory_bank_read`, `memory_bank_write`, `memory_bank_update`.
- **`search_memory` is not a memory-bank MCP tool**: use orchestrator `/memory/search` for federated recall (Qdrant + Mongo raw + MindsDB + Letta + memory-bank lexical fallback), or use Qdrant MCP (`/qdrant/mcp` with `qdrant-find`) directly when needed.
- **MCP requests need protocol headers**: include `Accept: application/json, text/event-stream`, `Content-Type: application/json`, and `MCP-Protocol-Version: 2025-11-25`.
- **Default local hub config is often lite**: if `.env` has `MCP_HUB_CONFIG=./configs/mcp-hub.lite.json`, `/mindsdb/mcp` is expected to be unavailable even when MindsDB container is running.

## Common Commands

### Startup & Lifecycle

```bash
# Full orchestrated launch (ollama → compose → router → mlx → init)
make launch          

# Core services only (detached)
gmake mem           # or: make mem-up
gmake mem-ps        # service status
gmake mem-logs      # follow logs

# Bring up with specific profiles
make up PROFILES=core,llm

# Stop everything
make down
gmake mem-down

# Restart services
gmake mem-restart
```

### Health & Diagnostics

```bash
# HTTP smoke test (MCP initialize + tools/list)
gmake mem-ping

# Multi-endpoint health probe
make doctor

# Orchestrator + trading telemetry
curl -fsS http://127.0.0.1:8075/status | jq
```

### Orchestrator (ContextLattice decision logging)

```bash
# Log a decision
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export MEMMCP_PROJECT=mem_mcp_lobehub

curl -fsS "$MEMMCP_ORCHESTRATOR_URL/memory/write" \
  -H 'content-type: application/json' \
  -d '{
    "projectName": "mem_mcp_lobehub",
    "fileName": "decisions/20251217_example.txt",
    "content": "Description of change, files affected, status"
  }'

# Read a file
curl -fsS "$MEMMCP_ORCHESTRATOR_URL/memory/files/mem_mcp_lobehub/decisions/20251217_example.txt"
```

### Direct MCP Hub Access (advanced)

```bash
# JSON-RPC initialize
curl -fsS http://127.0.0.1:53130/memorymcp/mcp \
  -H 'accept: application/json, text/event-stream' \
  -H 'content-type: application/json' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{
    "jsonrpc":"2.0",
    "id":"init-1",
    "method":"initialize",
    "params":{
      "protocolVersion":"2025-11-25",
      "clientInfo":{"name":"kalliste-alpha","version":"dev"}
    }
  }' | jq

# Tools list
curl -fsS http://127.0.0.1:53130/memorymcp/mcp \
  -H 'accept: application/json, text/event-stream' \
  -H 'content-type: application/json' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{"jsonrpc":"2.0","id":"tools-1","method":"tools/list","params":{}}' | jq
```

### Local LLM Stack

```bash
# Bring up Ollama (via Homebrew service or nohup)
make ollama-up
make ollama-wait

# Pull models (if ollama container is used)
make models-pull

# OpenAI-compatible router (combines Ollama + MLX)
make router-up
make router-status
make router-logs

# MLX server (Apple Silicon only)
make mlx-up
make mlx-down
```

### Development Dashboard

The Next.js dashboard (`memmcp-dashboard/`) provides a web UI for memory projects, file browsing, and health checks.

```bash
cd memmcp-dashboard
npm install
MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075 npm run dev
# Open http://localhost:3000
```

### Trae Agent (ByteDance multi-agent system)

```bash
# Install & configure
make trae-install
make trae-config   # renders trae_config.yaml from template

# Run agents
make trae-run-small   # fast_fix agent
make trae-run-big     # full agent
make trae-shell       # interactive mode
```

## Configuration

- **`.env`**: All service ports, API keys, Qdrant tuning (HNSW, quantization), embedding config
- **`configs/mcp-hub.config.json`**: mcp-proxy routing for MCP servers
- **`infra/compose/*.yml`**: Docker Compose overrides (port remapping, volumes, health checks, etc.)
- **`mk/memory.mk`**: Makefile targets for memory stack (included by main Makefile)

### Environment Variable Hierarchy

Compose reads `.env` from the **project directory** (the folder of the first `-f` file). A symlink `infra/compose/.env → ../../.env` ensures consistency when using `-f infra/compose/*.yml` files.

## Key Conventions

- **Qdrant internal URL**: Always use `http://qdrant:6333` from containers. Host access (if needed) is `:6334`.
- **MCP Protocol Version**: `2025-11-25` (set in headers `MCP-Protocol-Version`).
- **Decision file naming**: `decisions/YYYYMMDD_context.txt` for chronological sorting.
- **Handoff files**: `briefings/YYYYMMDD_projectname.txt` for session summaries.
- **Never commit without explicit user request**: When making commits, include `Co-Authored-By: Warp <agent@warp.dev>` at end of commit message.

## Testing & Verification

- **HTTP smoke ping**: `gmake mem-ping` (runs initialize + tools/list against `:53130/memorymcp/mcp`)
- **Orchestrator health**: `curl http://127.0.0.1:8075/status`
- **Service logs**: `gmake mem-logs` or `docker compose logs -f <service>`
- **Router health**: `make router-status` (checks `/v1/models`)

## Deployment

- **Local dev**: `make launch` (default)
- **Core only**: `make up-core` or `PROFILES=core make up`
- **Hosted core**: `scripts/deploy_hosted_core.sh <domain> <email>` (brings up Caddy reverse proxy with HTTPS for `/mcp` and `/status`)

## Troubleshooting

- **`.env` variables blank**: Ensure Compose project directory is correct; symlink `.env` into `infra/compose/` or use `--project-directory . --env-file .env`.
- **Port 6333 conflict**: Qdrant uses internal DNS. If host port needed, override to `:6334:6333` (older Compose) or publish manually.
- **Services stuck at "starting"**: Check logs via `docker compose logs -f memorymcp-http mcp-qdrant mindsdb-http-proxy`.
- **`/memorymcp/mcp` returns 404 after restart**: Restart `mcp-hub` after `memorymcp-http` and `mcp-qdrant` are healthy, or ensure `mcp-hub` waits for those backends before starting.
- **`/memory/write` returns 422**: Verify payload includes `projectName` and `fileName`; `content` alone is invalid.
- **Langfuse errors**: Verify ClickHouse password, migration URL, `NEXTAUTH_URL`, `SALT`, and `CLICKHOUSE_CLUSTER_ENABLED=false` in `.env`.

## License & Business Model

- **License**: Business Source License 1.1 (transitions to Apache-2.0 in 2028)
- **Additional Use Grant**: Personal/internal use up to 2M JSON-RPC calls/month
- **Commercial use**: Managed service offerings require separate license (contact Shea)
- **Roadmap**: metering, quotas, SSO/SAML, RBAC, billing (see `docs/ROADMAP.md`)

## Agent Memory Logging (CRITICAL)

### Global vs Project-Specific Context

ContextLattice uses a **hierarchical structure** for agent knowledge:

```
_global/                      # Cross-project protocols & learnings
├── agent_protocols/          # How ALL agents should behave
├── shared_learnings/         # Patterns that work across projects
└── infrastructure/           # Shared deployment/infra decisions

{projectName}/                # Project-specific context
├── decisions/                # Technical decisions for this project
├── briefings/                # Session handoffs
└── conventions/              # Project-specific patterns
```

### When to Log Where

**Log to `_global/`** when:
- Updating agent protocols (e.g., new logging convention)
- Discovering patterns that apply across projects
- Making infrastructure decisions affecting multiple projects

**Log to `{projectName}/`** when:
- Making technical decisions specific to this codebase
- Documenting project-specific test results or deployments
- Writing session handoffs for this project

### Logging Example

```bash
# Project-specific decision
curl -fsS "$MEMMCP_ORCHESTRATOR_URL/memory/write" \
  -H 'content-type: application/json' \
  -d '{
    "projectName": "mem_mcp_lobehub",
    "fileName": "decisions/'$(date +%Y%m%d)'_your_change.txt",
    "content": "- Changed X in file Y\n- Status: tests passed\n- Next: deploy to staging"
  }'

# Global protocol update
curl -fsS "$MEMMCP_ORCHESTRATOR_URL/memory/write" \
  -H 'content-type: application/json' \
  -d '{
    "projectName": "_global",
    "fileName": "agent_protocols/testing_standards.md",
    "content": "# Testing Standards\n\nAll agents should run smoke tests before committing..."
  }'
```

Failure to log means the next agent starts from scratch. See `docs/agent_protocols/logging_protocol.md` for full protocol.
