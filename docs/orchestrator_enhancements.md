# Orchestrator Service Enhancements

## Overview
The orchestrator now handles fanout durability and **federated retrieval** across memory services, while keeping `POST /memory/write` low-latency and resilient under partial service failures.

## Retrieval

### Federated Search (`POST /memory/search`)
Retrieval is merged across:
- `qdrant` semantic recall
- `mongo_raw` raw write event store
- `mindsdb` autosync table search
- `letta` archival memory search
- `memory_bank` lexical fallback scanning

The orchestrator deduplicates, applies source weights, and can rerank with learning preferences from feedback records.

**Request**
```json
{
  "query": "helius RPC configuration",
  "limit": 10,
  "project": "algotraderv2_rust",
  "topic_path": "decisions",
  "fetch_content": false,
  "sources": ["qdrant", "mongo_raw", "letta"],
  "source_weights": {"qdrant": 1.0, "letta": 0.9},
  "rerank_with_learning": true,
  "include_retrieval_debug": true
}
```

**Response**
```json
{
  "results": [
    {
      "project": "algotraderv2_rust",
      "file": "decisions/20260210_rpc.md",
      "summary": "Configured Helius RPC as primary...",
      "score": 0.89,
      "source": "qdrant",
      "sources": ["mongo_raw", "qdrant"]
    }
  ],
  "warnings": [],
  "retrieval": {
    "sources": ["qdrant", "mongo_raw", "letta"],
    "source_counts": {"qdrant": 10, "mongo_raw": 7, "letta": 4},
    "source_errors": {},
    "learning_rerank": {"enabled": true}
  }
}
```

## Write + Fanout

### Memory Write (`POST /memory/write`)
Writes remain memory-bank-first, then fan out durably to:
- `mongo_raw`
- `qdrant`
- `mindsdb`
- `letta`
- `langfuse` (if configured)

The response includes warnings when fanout is degraded, so callers can detect partial durability and rely on queued retries.

### Fanout Outbox Backend
The outbox now supports:
- `sqlite` (default)
- `mongo` (recommended for multi-instance/HA deployments)

`GET /telemetry/fanout` and `GET /telemetry/memory` now expose `outboxBackend`.

### Fanout Coalescer (Hot-path write reduction)
Repeated writes to the same `project/file` can now be coalesced into a single pending outbox job per target within a short window.

Key knobs:
- `FANOUT_COALESCE_ENABLED`
- `FANOUT_COALESCE_WINDOW_SECS`
- `FANOUT_COALESCE_TARGETS`

Telemetry:
- `GET /telemetry/fanout` -> `coalescer`
- `GET /telemetry/memory` -> `fanout.coalescer`

### Letta Admission Control (Backlog-aware)
When Letta backlog is high, low-value writes (for example hot rollups and `__latest` telemetry snapshots) are skipped for Letta fanout while still preserving memory-bank + raw durability.

Key knobs:
- `LETTA_ADMISSION_ENABLED`
- `LETTA_ADMISSION_BACKLOG_SOFT_LIMIT`
- `LETTA_ADMISSION_BACKLOG_HARD_LIMIT`
- `LETTA_ADMISSION_LOW_VALUE_MIN_SUMMARY_CHARS`

Telemetry:
- `GET /telemetry/fanout` -> `lettaAdmission`
- `GET /telemetry/memory` -> `fanout.letta.admission`

### Sink Retention Worker (Qdrant/Mongo/Letta)
The orchestrator can run an in-process low-value retention pass for sink growth control.

Key knobs:
- `SINK_RETENTION_ENABLED`
- `SINK_RETENTION_INTERVAL_SECS`
- `SINK_RETENTION_TIMEOUT_SECS`
- `SINK_RETENTION_SCAN_LIMIT`
- `SINK_RETENTION_MAX_DELETES_PER_RUN`
- `QDRANT_LOW_VALUE_RETENTION_HOURS`
- `MONGO_RAW_LOW_VALUE_RETENTION_HOURS` (defaults to `0`, disabled)
- `LETTA_LOW_VALUE_RETENTION_HOURS`

Endpoints:
- `GET /telemetry/retention`
- `POST /telemetry/retention/run`

## MindsDB Analytics

### Trading Analytics (`GET /analytics/trading`)
Trading analytics now query MindsDB SQL directly from `MINDSDB_TRADING_DB.MINDSDB_TRADING_TABLE`.

`POST /telemetry/trading` also syncs snapshots to that table (best-effort), enabling real SQL-backed rollups.

## Security Guardrails

When `MEMMCP_ENV=production` and `ORCH_SECURITY_STRICT=true`:
- `MEMMCP_ORCHESTRATOR_API_KEY` is required
- optional public exposure for `/status` and docs is controlled via:
  - `ORCH_PUBLIC_STATUS`
  - `ORCH_PUBLIC_DOCS`

Use:
```bash
scripts/security_preflight.sh
```
before production launch.

## Key Environment Variables

```yaml
# retrieval
ORCH_RETRIEVAL_SOURCES: qdrant,mongo_raw,mindsdb,letta,memory_bank
ORCH_RETRIEVAL_ENABLE_LEARNING_RERANK: "true"

# embeddings (free local default)
ORCH_EMBED_PROVIDER: ollama
ORCH_EMBED_MODEL: nomic-embed-text:latest
ORCH_EMBED_FAIL_OPEN: "true"

# mindsdb trading analytics
MINDSDB_TRADING_AUTOSYNC: "true"
MINDSDB_TRADING_DB: files
MINDSDB_TRADING_TABLE: trading_metrics

# fanout outbox backend
FANOUT_OUTBOX_BACKEND: sqlite # or mongo
FANOUT_OUTBOX_MONGO_URI: mongodb://mongo:27017
FANOUT_OUTBOX_MONGO_DB: memmcp_raw
FANOUT_OUTBOX_MONGO_COLLECTION: fanout_outbox

# production security
MEMMCP_ENV: development
ORCH_SECURITY_STRICT: "true"
ORCH_PUBLIC_STATUS: "true"
ORCH_PUBLIC_DOCS: "true"
```

## Quick Checks

```bash
# Service health
curl -fsS http://127.0.0.1:8075/health | jq

# Federated retrieval
curl -fsS http://127.0.0.1:8075/memory/search \
  -H 'content-type: application/json' \
  -d '{"query":"rpc provider","limit":5,"include_retrieval_debug":true}' | jq

# Fanout backend + queue state
curl -fsS http://127.0.0.1:8075/telemetry/fanout | jq

# MindsDB analytics
curl -fsS http://127.0.0.1:8075/analytics/trading | jq
```
