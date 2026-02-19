# On‑prem full runbook

This runbook covers upgrade, backup, and retention procedures for the full on‑prem stack.

## Upgrade flow

1. Pull the latest repo or release bundle.
2. Copy updated `.env.example` values into your `.env` (do not overwrite secrets).
3. Restart the stack:
   ```bash
   gmake mem-down
   gmake mem-up
   ```
4. Verify health:
   ```bash
   curl -fsS http://127.0.0.1:8075/health
   curl -fsS http://127.0.0.1:8075/status | jq
   ```

## Backup flow

Run these on a schedule (weekly or daily depending on policy):

```bash
scripts/retention_runner.sh
```

This performs:
- telemetry archive
- qdrant snapshot prune
- memory bank export + purge (if enabled)

Recommended backup targets:
- `MEMORY_BANK_EXPORT_DIR`
- Qdrant snapshots (see `docs/storage_and_retention.md`)
- Orchestrator data dir (`ORCHESTRATOR_DATA`)

## Restore flow

1. Restore exported memory files to the memory bank volume.
2. Restore Qdrant snapshot (see `scripts/qdrant_snapshot_prune.py` docs).
3. Restore orchestrator data dir (trading metrics, history, etc.).
4. Restart the stack.

## Retention policy (regulated clients)

- Set `MEMORY_BANK_RETENTION_DAYS` to the retention requirement.
- Enable audit logging for retention actions:
  - `MEMMCP_DASHBOARD_URL`
  - `MEMMCP_DASHBOARD_API_KEY` (with `audit:write`)
- Document retention policy and change windows in client SLA.
