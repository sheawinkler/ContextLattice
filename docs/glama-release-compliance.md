# Glama Release Compliance

This guide defines the exact settings for Glama release configuration.

## Dockerfile release settings

Use these in the Glama Dockerfile admin form:

- Dockerfile path: `Dockerfile.orchestrator`
- Build context: `.`
- Command: use Dockerfile default command
- Port: `8075`
- Health endpoint: `/health`
- Build steps: `[]` (leave empty when Dockerfile path is set)
- CMD arguments: `[]` (use Dockerfile default command)

These are deploy settings, not environment variables.

## Python version note (3.14 vs 3.12)

- Glama generated builds can succeed on Python `3.14` when compatible wheels are available.
- For deterministic container reproducibility across platforms, the repo Dockerfile lanes are pinned to Python `3.12`.
- If you use generated mode, prefer Python `3.12` unless you explicitly need `3.14` and have validated dependencies in your target runtime.

## Generated Dockerfile Mode (UI fallback)

If Glama forces generated Dockerfile mode with a fixed `mcp-proxy --` prefix, use:

- Build steps:
  - `python -m venv /opt/venv`
  - `/opt/venv/bin/pip install --upgrade pip setuptools wheel`
  - `/opt/venv/bin/pip install --no-cache-dir -r services/orchestrator/requirements.txt`
- CMD arguments:
  - `/opt/venv/bin/python`
  - `services/orchestrator/mcp_stdio_server.py`

`mcp_stdio_server.py` starts the local orchestrator HTTP runtime and exposes MCP over stdio
so `mcp-proxy` can complete initialize/tools/list/tool-call checks.

### Copy-paste snippets (Generated mode)

Build steps:

```json
[
  "python -m venv /opt/venv",
  "/opt/venv/bin/pip install --upgrade pip setuptools wheel",
  "/opt/venv/bin/pip install --no-cache-dir -r services/orchestrator/requirements.txt"
]
```

CMD arguments:

```json
[
  "mcp-proxy",
  "--",
  "/opt/venv/bin/python",
  "services/orchestrator/mcp_stdio_server.py"
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
    "ORCH_SECURITY_STRICT": {
      "default": "false",
      "description": "If true, missing production posture checks fail startup.",
      "type": "string"
    },
    "ORCH_PRODUCTION_REQUIRE_API_KEY": {
      "default": "false",
      "description": "If true, production mode requires CONTEXTLATTICE_ORCHESTRATOR_API_KEY.",
      "type": "string"
    },
    "SECRETS_STORAGE_MODE": {
      "default": "redact",
      "description": "Secret-like data handling policy.",
      "type": "string"
    },
    "ORCH_START_INTERNAL": {
      "default": "true",
      "description": "Start internal orchestrator for tool calls.",
      "type": "string"
    },
    "ORCH_STARTUP_TIMEOUT_SECS": {
      "default": "45",
      "description": "Seconds to wait for internal orchestrator health during tool calls.",
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
    "ORCH_RETRIEVAL_DEFAULT_SOURCES": {
      "default": "topic_rollups",
      "description": "Default retrieval lane list.",
      "type": "string"
    },
    "ORCH_RETRIEVAL_FAST_SOURCES": {
      "default": "topic_rollups",
      "description": "Fast retrieval lane list.",
      "type": "string"
    },
    "TOPIC_ROLLUP_SQLITE_ENABLED": {
      "default": "true",
      "description": "Enable sqlite rollup index.",
      "type": "string"
    },
    "TOPIC_ROLLUP_SQLITE_FTS_ENABLED": {
      "default": "true",
      "description": "Enable sqlite FTS5/BM25 lexical acceleration.",
      "type": "string"
    },
    "TOPIC_ROLLUP_SQLITE_VEC_ENABLED": {
      "default": "true",
      "description": "Enable optional sqlite-vec acceleration when available.",
      "type": "string"
    },
    "SIGNAL_REFRESH_ENABLED": {
      "default": "false",
      "description": "Disable signal refresh loop in Glama single-container mode.",
      "type": "string"
    },
    "OVERRIDE_REFRESH_ENABLED": {
      "default": "false",
      "description": "Disable override refresh loop in Glama single-container mode.",
      "type": "string"
    },
    "SINK_RETENTION_ENABLED": {
      "default": "false",
      "description": "Disable sink retention worker in Glama single-container mode.",
      "type": "string"
    }
  },
  "required": [],
  "type": "object"
}
```

Placeholder parameters (first deploy / dev-safe):

```json
{
  "CONTEXTLATTICE_ENV": "development",
  "ORCH_SECURITY_STRICT": "false",
  "ORCH_PRODUCTION_REQUIRE_API_KEY": "false",
  "SECRETS_STORAGE_MODE": "redact",
  "ORCH_START_INTERNAL": "true",
  "ORCH_STARTUP_TIMEOUT_SECS": "45",
  "FANOUT_OUTBOX_BACKEND": "sqlite",
  "MONGO_RAW_ENABLED": "false",
  "MINDSDB_ENABLED": "false",
  "ORCH_PGVECTOR_ENABLED": "false",
  "ORCH_RETRIEVAL_DEFAULT_SOURCES": "topic_rollups",
  "ORCH_RETRIEVAL_FAST_SOURCES": "topic_rollups",
  "TOPIC_ROLLUP_SQLITE_ENABLED": "true",
  "TOPIC_ROLLUP_SQLITE_FTS_ENABLED": "true",
  "TOPIC_ROLLUP_SQLITE_VEC_ENABLED": "true",
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
  "ORCH_SECURITY_STRICT": "true",
  "ORCH_PRODUCTION_REQUIRE_API_KEY": "true",
  "SECRETS_STORAGE_MODE": "redact",
  "ORCH_START_INTERNAL": "true",
  "ORCH_STARTUP_TIMEOUT_SECS": "45",
  "FANOUT_OUTBOX_BACKEND": "sqlite",
  "MONGO_RAW_ENABLED": "false",
  "MINDSDB_ENABLED": "false",
  "ORCH_PGVECTOR_ENABLED": "false",
  "ORCH_RETRIEVAL_DEFAULT_SOURCES": "topic_rollups",
  "ORCH_RETRIEVAL_FAST_SOURCES": "topic_rollups",
  "TOPIC_ROLLUP_SQLITE_ENABLED": "true",
  "TOPIC_ROLLUP_SQLITE_FTS_ENABLED": "true",
  "TOPIC_ROLLUP_SQLITE_VEC_ENABLED": "true",
  "SIGNAL_REFRESH_ENABLED": "false",
  "OVERRIDE_REFRESH_ENABLED": "false",
  "SINK_RETENTION_ENABLED": "false"
}
```

Important typo guard: use `MONGO_RAW_ENABLED` exactly (not `MONGO_RAW_ENABLEDI`).

## Standalone Service Profile

`Dockerfile.orchestrator` now defaults to a standalone-safe profile for Glama:

- disables external service dependencies that are not present in a single container (`mongo`, `mindsdb`, `pgvector`)
- uses sqlite fanout outbox
- disables signal/override polling loops that rely on external memory services
- keeps retrieval on `topic_rollups` as the default fast lane
- enables sqlite topic-rollup acceleration with WAL + FTS5 BM25 scoring
- auto-detects optional `sqlite-vec` module and keeps retrieval fail-open when unavailable

## Arguments JSON schema

Use the schema box for runtime variables only.

Use `CONTEXTLATTICE_*` names in Glama.

- `CONTEXTLATTICE_ENV`: `development` (default) or `production`
- `CONTEXTLATTICE_ORCHESTRATOR_API_KEY`: required only when `CONTEXTLATTICE_ENV=production`
- `SECRETS_STORAGE_MODE`: `redact` (default), `block`, or `allow`
- `MESSAGING_OPENCLAW_STRICT_SECURITY`: optional boolean string
- `IRONCLAW_INTEGRATION_ENABLED`: optional boolean string
- `IRONCLAW_DEFAULT_PROJECT`: optional string
- `TOPIC_ROLLUP_SQLITE_ENABLED`: defaults `true` for Glama-lite
- `TOPIC_ROLLUP_SQLITE_FTS_ENABLED`: defaults `true` (FTS5/BM25 lane)
- `TOPIC_ROLLUP_SQLITE_VEC_ENABLED`: defaults `true` (auto capability-detect only)

Legacy `MEMMCP_*` names are no longer the canonical public configuration.

## Python Version Compatibility

Release `v3.2.9` updates orchestrator dependency pins for CPython 3.14 build images:

- `pydantic==2.12.5`
- `orjson==3.11.7`

## Timing for score updates

- Repo metadata checks (README/license/glama.json): usually 10-60 minutes, sometimes longer.
- Quality/security checks: update after successful Glama Deploy + Release.
