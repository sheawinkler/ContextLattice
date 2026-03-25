# Hugging Face Docker Space (Lite)

This guide deploys the single-container lite lane on Hugging Face Spaces with low drift risk and reproducible behavior.

## What this repo now provides

- `Dockerfile.hf-lite` with pinned runtime defaults for single-container operation.
- pinned Python dependencies in `services/orchestrator/requirements.txt`.
- root probe (`GET /`) and health probe (`GET /health`) in `services/orchestrator/app.py`.
- `.dockerignore` to keep build context small and deterministic.

## Why Python 3.12 here

Glama generated builds can run on Python 3.14 when upstream wheels are available, but 3.12 is the safest baseline for reproducible Docker Space builds across registries and mirrors.

## Space setup (manual UI steps)

1. Create a new Hugging Face Space.
2. Choose SDK: `Docker`.
3. Push this repository branch to the Space repo.
4. Ensure Dockerfile path is `Dockerfile.hf-lite`.
5. In Space variables, set:
   - `PORT=7860`
6. Optional secret for strict mode:
   - `CONTEXTLATTICE_ORCHESTRATOR_API_KEY=<your-key>`

## Recommended runtime variables

Default deterministic-lite profile (copy as regular variables):

```env
CONTEXTLATTICE_ENV=development
ORCH_SECURITY_STRICT=false
ORCH_PRODUCTION_REQUIRE_API_KEY=false
FANOUT_OUTBOX_BACKEND=sqlite
MONGO_RAW_ENABLED=false
MINDSDB_ENABLED=false
ORCH_PGVECTOR_ENABLED=false
ORCH_RETRIEVAL_DEFAULT_SOURCES=topic_rollups
ORCH_RETRIEVAL_FAST_SOURCES=topic_rollups
TOPIC_ROLLUP_SQLITE_ENABLED=true
TOPIC_ROLLUP_SQLITE_FTS_ENABLED=true
TOPIC_ROLLUP_SQLITE_VEC_ENABLED=true
TOPIC_ROLLUP_SQLITE_PATH=/data/topic_rollups.sqlite3
TASK_DB_PATH=/data/agent_tasks.db
FANOUT_OUTBOX_PAYLOAD_BLOB_DIR=/data/fanout_payload_blobs
ORCH_STORAGE_GOVERNANCE_DISK_ROOT=/data
ORCH_STORAGE_GOVERNANCE_ENABLED=true
ORCH_STORAGE_GOVERNANCE_RUN_ON_STARTUP=true
SIGNAL_REFRESH_ENABLED=false
OVERRIDE_REFRESH_ENABLED=false
SINK_RETENTION_ENABLED=false
```

Strict production posture (optional):

```env
CONTEXTLATTICE_ENV=strict
ORCH_SECURITY_STRICT=true
ORCH_PRODUCTION_REQUIRE_API_KEY=true
CONTEXTLATTICE_ORCHESTRATOR_API_KEY=<secret>
```

## Drift controls

- Use persistent storage (`/data`) if you want cache continuity.
- If you want maximum determinism, disable learning adaptation:
  - `LEARNING_LOOP_ENABLED=false`
- Trigger a manual compaction/governance run when needed:
  - `POST /maintenance/storage/run`
- Inspect storage state:
  - `GET /telemetry/storage`

## Local verification before push

```bash
docker build -f Dockerfile.hf-lite -t contextlattice-hf-lite .
docker run --rm -p 7860:7860 contextlattice-hf-lite
curl -s http://127.0.0.1:7860/
curl -s http://127.0.0.1:7860/health
```
