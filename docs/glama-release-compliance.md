# Glama Release Compliance

This guide defines the exact deployment settings for Glama.

## Preferred: Repo Dockerfile mode (Go backend)

Use these in the Glama Dockerfile admin form:

- Dockerfile path: `Dockerfile.orchestrator`
- Build context: `.`
- Command: use Dockerfile default command
- Port: `8075`
- Health endpoint: `/health`
- Build steps: `[]` (leave empty when Dockerfile path is set)
- CMD arguments: `[]` (use Dockerfile default command)

`Dockerfile.orchestrator` now builds and runs `gateway-go` directly with standalone-safe defaults.

## Generated Dockerfile mode (UI fallback)

If Glama forces generated Dockerfile mode with a fixed `mcp-proxy --` prefix, use Go-first startup:

- Build steps:
  - `apt-get update && apt-get install -y --no-install-recommends golang-go && rm -rf /var/lib/apt/lists/*`
  - `go build -C services/gateway-go -o /opt/contextlattice-gateway-go .`
- CMD arguments:
  - `mcp-proxy`
  - `--`
  - `python`
  - `archive/services/orchestrator_legacy_python/mcp_stdio_server.py`

In this mode the Python bridge is transport-only: it starts `gateway-go` and forwards MCP calls to `http://127.0.0.1:8075`.

### Copy-paste snippets (generated mode)

Build steps:

```json
[
  "apt-get update && apt-get install -y --no-install-recommends golang-go && rm -rf /var/lib/apt/lists/*",
  "go build -C services/gateway-go -o /opt/contextlattice-gateway-go ."
]
```

CMD arguments:

```json
[
  "mcp-proxy",
  "--",
  "python",
  "archive/services/orchestrator_legacy_python/mcp_stdio_server.py"
]
```

Environment variables JSON schema:

```json
{
  "properties": {
    "CONTEXTLATTICE_ENV": {
      "default": "development",
      "description": "Runtime mode: development | production | strict.",
      "type": "string"
    },
    "CONTEXTLATTICE_ORCHESTRATOR_API_KEY": {
      "description": "Required when CONTEXTLATTICE_ENV is production/strict and API-key requirement is enabled.",
      "type": "string"
    },
    "ORCH_INTERNAL_RUNTIME": {
      "default": "gateway-go",
      "description": "Internal runtime for stdio bridge: gateway-go | python-orchestrator | auto.",
      "type": "string"
    },
    "ORCH_GATEWAY_BIN": {
      "default": "/opt/contextlattice-gateway-go",
      "description": "Absolute path to gateway-go binary in generated mode.",
      "type": "string"
    },
    "ORCH_START_INTERNAL": {
      "default": "true",
      "description": "Start internal runtime for tool calls.",
      "type": "string"
    },
    "ORCH_STARTUP_TIMEOUT_SECS": {
      "default": "45",
      "description": "Seconds to wait for internal runtime health.",
      "type": "string"
    },
    "GO_RUNTIME_STRICT_NO_PYTHON": {
      "default": "true",
      "description": "Disallow Python backend fallback on hot paths.",
      "type": "string"
    },
    "GO_PYTHON_HOT_PATH_OWNERSHIP_MODE": {
      "default": "strict",
      "description": "Ownership gate for Python fallback lanes.",
      "type": "string"
    },
    "FANOUT_OUTBOX_BACKEND": {
      "default": "sqlite",
      "description": "Fanout outbox backend for single-container mode.",
      "type": "string"
    },
    "MONGO_RAW_ENABLED": {
      "default": "false",
      "description": "Disable mongo lane in Glama single-container mode.",
      "type": "string"
    },
    "MINDSDB_ENABLED": {
      "default": "false",
      "description": "Disable MindsDB lane in Glama single-container mode.",
      "type": "string"
    },
    "ORCH_PGVECTOR_ENABLED": {
      "default": "false",
      "description": "Disable pgvector lane in Glama single-container mode.",
      "type": "string"
    },
    "ORCH_RETRIEVAL_SOURCES": {
      "default": "topic_rollups",
      "description": "Canonical retrieval lane list for the Go gateway.",
      "type": "string"
    },
    "ORCH_RETRIEVAL_DEFAULT_SOURCES": {
      "default": "topic_rollups",
      "description": "Legacy retrieval lane alias kept for older launchers.",
      "type": "string"
    },
    "ORCH_RETRIEVAL_FAST_SOURCES": {
      "default": "topic_rollups",
      "description": "Fast retrieval lane list.",
      "type": "string"
    },
    "SIGNAL_REFRESH_ENABLED": {
      "default": "false",
      "description": "Disable signal refresh loop in single-container mode.",
      "type": "string"
    },
    "OVERRIDE_REFRESH_ENABLED": {
      "default": "false",
      "description": "Disable override refresh loop in single-container mode.",
      "type": "string"
    },
    "SINK_RETENTION_ENABLED": {
      "default": "false",
      "description": "Disable sink retention worker in single-container mode.",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

Placeholder parameters (dev-safe):

```json
{
  "CONTEXTLATTICE_ENV": "development",
  "ORCH_INTERNAL_RUNTIME": "gateway-go",
  "ORCH_GATEWAY_BIN": "/opt/contextlattice-gateway-go",
  "ORCH_START_INTERNAL": "true",
  "ORCH_STARTUP_TIMEOUT_SECS": "45",
  "GO_RUNTIME_STRICT_NO_PYTHON": "true",
  "GO_PYTHON_HOT_PATH_OWNERSHIP_MODE": "strict",
  "FANOUT_OUTBOX_BACKEND": "sqlite",
  "MONGO_RAW_ENABLED": "false",
  "MINDSDB_ENABLED": "false",
  "ORCH_PGVECTOR_ENABLED": "false",
  "ORCH_RETRIEVAL_SOURCES": "topic_rollups",
  "ORCH_RETRIEVAL_DEFAULT_SOURCES": "topic_rollups",
  "ORCH_RETRIEVAL_FAST_SOURCES": "topic_rollups",
  "SIGNAL_REFRESH_ENABLED": "false",
  "OVERRIDE_REFRESH_ENABLED": "false",
  "SINK_RETENTION_ENABLED": "false"
}
```

Placeholder parameters (production strict with key):

```json
{
  "CONTEXTLATTICE_ENV": "strict",
  "CONTEXTLATTICE_ORCHESTRATOR_API_KEY": "replace-with-real-key",
  "ORCH_INTERNAL_RUNTIME": "gateway-go",
  "ORCH_GATEWAY_BIN": "/opt/contextlattice-gateway-go",
  "ORCH_START_INTERNAL": "true",
  "ORCH_STARTUP_TIMEOUT_SECS": "45",
  "GO_RUNTIME_STRICT_NO_PYTHON": "true",
  "GO_PYTHON_HOT_PATH_OWNERSHIP_MODE": "strict",
  "FANOUT_OUTBOX_BACKEND": "sqlite",
  "MONGO_RAW_ENABLED": "false",
  "MINDSDB_ENABLED": "false",
  "ORCH_PGVECTOR_ENABLED": "false",
  "ORCH_RETRIEVAL_SOURCES": "topic_rollups",
  "ORCH_RETRIEVAL_DEFAULT_SOURCES": "topic_rollups",
  "ORCH_RETRIEVAL_FAST_SOURCES": "topic_rollups",
  "SIGNAL_REFRESH_ENABLED": "false",
  "OVERRIDE_REFRESH_ENABLED": "false",
  "SINK_RETENTION_ENABLED": "false"
}
```

Important typo guard: use `MONGO_RAW_ENABLED` exactly (not `MONGO_RAW_ENABLEDI`).

## Standalone profile summary

Glama single-container profile is tuned for deterministic startup:

- no external service dependency (`mongo`, `mindsdb`, `pgvector` disabled)
- sqlite fanout outbox
- `topic_rollups` default/fast retrieval lane
- Go runtime strict mode (`GO_RUNTIME_STRICT_NO_PYTHON=true`)

## Legacy Python fallback mode (only if required)

Only use this if Go compilation is blocked in generated mode:

- Build steps:
  - `python -m venv /opt/venv`
  - `/opt/venv/bin/pip install --upgrade pip setuptools wheel`
  - `/opt/venv/bin/pip install --no-cache-dir -r archive/services/orchestrator_legacy_python/requirements.txt`
- Placeholder parameter override:
  - `"ORCH_INTERNAL_RUNTIME": "python-orchestrator"`

## Timing for score updates

- Repo metadata checks (README/license/glama.json): usually 10-60 minutes.
- Quality/security checks: update after successful Glama Deploy + Release.
