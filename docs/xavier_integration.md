# Xavier Mode Telemetry Notes

This repo now has a lightweight way to push trading context into the memMCP
stack without blocking the trading loop. The Rust agent uses
`TelemetryClient` (see `~/Documents/Projects/algotraderv2_rust/src/monitoring/telemetry.rs`) to
buffer events and asynchronously POST them to this orchestrator via
`/memory/write`.

## Key points
- Set `MEMMCP_ORCHESTRATOR_URL` (default `http://127.0.0.1:8075`) and
  optionally `MEMMCP_PROJECT` before launching `xavier_mode`.
- Tune batching with `MEMMCP_BATCH_SIZE` (default 5) and
  `MEMMCP_BATCH_INTERVAL_MS` (default 2000). Set
  `MEMMCP_LOCAL_BACKUP_DIR` to keep an NDJSON mirror of every event in case
  the orchestrator is down.
- For durability, set `MEMMCP_LOCAL_STORE_PATH=/path/to/spool` to enable the
  sled-backed hot-path cache. Every telemetry event is written locally first,
  retried automatically, and removed once the orchestrator confirms the batch.
  Watching the `local_store_queue_depth` gauge (exported via the metrics feed)
  makes it easy to alert when the spool grows faster than the exporter drains it.
- Export `MEMMCP_METRICS_URL` if you need to forward telemetry counters to a
  custom endpoint; by default it points to the orchestrator's
  `/telemetry/metrics` route that the dashboard consumes.
- Each run queues the loaded godmode profile and every sidecar guidance
  event; the background worker flushes them to memMCP, Qdrant, and
  Langfuse.
- Export `MEMMCP_DISABLE=1` to skip logging while keeping the code path.

## Example payload
When the trader loads a profile you will see requests like:

```json
{
  "projectName": "xavier-mode",
  "fileName": "telemetry/batch-20250102T010203123Z.json",
  "content": "{\n  \"timestamp\": \"2025-01-02T01:02:03Z\",\n  \"event_count\": 5,\n  \"events\": [ { ... } ]\n}"
}
```

Each batch bundles several events (profiles + sidecar guidance). If you
need per-event detail, inspect the NDJSON mirror written under
`$MEMMCP_LOCAL_BACKUP_DIR/telemetry.ndjson`.
