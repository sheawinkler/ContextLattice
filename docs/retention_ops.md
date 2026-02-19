# Retention operations

## Run retention jobs
Use the included runner (uses `gmake` targets internally):

```bash
scripts/retention_runner.sh
```

The runner now also calls:
- `scripts/memory_bank_export.sh`
- `scripts/memory_bank_purge.sh`
- `scripts/fanout_outbox_gc.py`

The orchestrator also runs built-in fanout outbox GC continuously when
`FANOUT_OUTBOX_GC_ENABLED=1` (default). You can trigger it on demand with:

```bash
curl -sS -X POST http://127.0.0.1:${ORCHESTRATOR_PORT:-8075}/telemetry/fanout/gc | jq
```

For sink-level low-value retention (Qdrant/Mongo/Letta), use:

```bash
curl -sS http://127.0.0.1:${ORCHESTRATOR_PORT:-8075}/telemetry/retention | jq
curl -sS -X POST http://127.0.0.1:${ORCHESTRATOR_PORT:-8075}/telemetry/retention/run | jq
```

## Suggested schedule
Run daily, weekly, hourly, or every 35 minutes (for high-write local dev). Example (every 35 minutes):

```
*/35 * * * * /path/to/mem_mcp_lobehub/scripts/retention_runner.sh
```

For macOS launchd automation (installed in `~/Library/LaunchAgents`):

```bash
RETENTION_INTERVAL_SECONDS=2100 scripts/install_retention_runner.sh install
scripts/install_retention_runner.sh status
```

Install a separate daily Qdrant snapshot job (recommended when `QDRANT_SKIP_SNAPSHOT=1` on high cadence):

```bash
QDRANT_SNAPSHOT_INTERVAL_SECONDS=86400 scripts/install_qdrant_snapshot_runner.sh install
scripts/install_qdrant_snapshot_runner.sh status
```

## Config
These env vars control retention:
- `RETENTION_INTERVAL_SECONDS` (launchd cadence in seconds, default `2100`)
- `QDRANT_SNAPSHOT_INTERVAL_SECONDS` (daily snapshot cadence in seconds, default `86400`)
- `QDRANT_RETENTION_DAYS`
- `QDRANT_RETENTION_HOURS` (if >0, overrides days)
- `QDRANT_RETENTION_ENABLED`
- `QDRANT_SKIP_SNAPSHOT` (default `1`; retention runner also auto-sets this for cadence `<=3600s` unless explicitly overridden)
- `QDRANT_SKIP_PRUNE`
- `QDRANT_HTTP_TIMEOUT_SECS`
- `TELEMETRY_HOT_HOURS`
- `MEMMCP_COLD_ROOT`
- `MEMORY_BANK_EXPORT_DIR`
- `MEMORY_BANK_PATH`
- `MEMORY_BANK_RETENTION_DAYS`
- `MEMORY_BANK_EXPORT_ENABLED`
- `MEMORY_BANK_PURGE_ENABLED`
- `MEMORY_BANK_PURGE_DRY_RUN`
- `MEMORY_BANK_PURGE_VERBOSE`
- `FANOUT_OUTBOX_GC_ENABLED`
- `FANOUT_OUTBOX_GC_INTERVAL_SECS` (orchestrator in-process cadence, default `900`)
- `FANOUT_OUTBOX_GC_DB_PATH`
- `FANOUT_OUTBOX_SUCCEEDED_RETENTION_HOURS`
- `FANOUT_OUTBOX_FAILED_RETENTION_HOURS`
- `FANOUT_OUTBOX_STALE_PENDING_HOURS`
- `FANOUT_OUTBOX_STALE_TARGETS` (optional comma list, e.g. `letta`, for stale pending/retrying/running cleanup)
- `FANOUT_OUTBOX_GC_VACUUM`
- `FANOUT_OUTBOX_GC_VACUUM_MIN_DELETED`
- `FANOUT_OUTBOX_GC_VACUUM_MIN_INTERVAL_SECS`
- `FANOUT_OUTBOX_GC_TIMEOUT_SECS`
- `FANOUT_OUTBOX_GC_DRY_RUN`
- `SINK_RETENTION_ENABLED`
- `SINK_RETENTION_INTERVAL_SECS`
- `SINK_RETENTION_TIMEOUT_SECS`
- `SINK_RETENTION_SCAN_LIMIT`
- `SINK_RETENTION_DELETE_BATCH`
- `SINK_RETENTION_MAX_DELETES_PER_RUN`
- `QDRANT_LOW_VALUE_RETENTION_HOURS`
- `MONGO_RAW_LOW_VALUE_RETENTION_HOURS` (set `0` to disable; default)
- `LETTA_LOW_VALUE_RETENTION_HOURS`
- `LETTA_RETENTION_PAGE_LIMIT`
- `LETTA_RETENTION_MAX_DELETES_PER_RUN`
- `LOW_VALUE_FILE_SUFFIXES`
- `LOW_VALUE_TOPIC_PREFIXES`
- `MEMMCP_DASHBOARD_URL`
- `MEMMCP_DASHBOARD_API_KEY` (requires an API key with `audit:write`)
