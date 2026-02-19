# Monitoring & alerts

## Orchestrator JSON logs

Set `ORCH_LOG_FILE` to capture JSON events emitted by the orchestrator:

```bash
ORCH_LOG_FILE=./tmp/orchestrator.jsonl
ORCH_LOG_LEVEL=INFO
```

## Stack watcher

The watcher polls `/status`, `/telemetry/metrics`, and `/telemetry/trading`, then
writes snapshots and alerts to JSONL.

```bash
python3 scripts/stack_watch.py --interval 10
```

Env overrides:
- `STACK_WATCH_OUT=./tmp/stack_watch.ndjson`
- `STACK_ALERT_OUT=./tmp/stack_alerts.ndjson`
- `ALERT_QUEUE_DEPTH=500`
- `ALERT_SERVICE_DOWN=1`
