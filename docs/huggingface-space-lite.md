# Hugging Face Docker Space (Lite, Go-native)

This guide deploys the single-container lite lane on Hugging Face Spaces using the Go gateway runtime.

## Resource profile (hf-lite lane)

- Target lane: Public `v3.3.x` compatibility profile.
- Baseline sizing: `2-4` vCPU, `4-8 GB` RAM, `20-50 GB` SSD.
- Recommended disk headroom: keep at least `10 GB` free in `/data` to avoid compaction/rewrite pressure.
- Retrieval default in this lane: `topic_rollups` (single-container-safe).

## What this repo now provides

- `Dockerfile.hf-lite` builds `services/gateway-go` and runs it directly on port `7860`.
- Runtime defaults for strict Go ownership and sqlite-safe lite mode.
- `.dockerignore` to keep build context small and deterministic.

## Space setup (manual UI steps)

1. Create a new Hugging Face Space.
2. Choose SDK: `Docker`.
3. Push this repository branch to the Space repo.
4. Hugging Face expects a root `Dockerfile`:
   - copy `Dockerfile.hf-lite` to `Dockerfile` in the Space repo before build.
5. In Space variables, set:
   - `PORT=7860`
6. Optional secret for strict mode:
   - `CONTEXTLATTICE_ORCHESTRATOR_API_KEY=<your-key>`

Example (inside the Space repo):

```bash
cp Dockerfile.hf-lite Dockerfile
git add Dockerfile
git commit -m "chore: use hf-lite dockerfile"
git push
```

## Recommended runtime variables

Default deterministic-lite profile (copy as regular variables):

```env
CONTEXTLATTICE_ENV=development
ORCH_SECURITY_STRICT=false
ORCH_PRODUCTION_REQUIRE_API_KEY=false
GO_RUNTIME_STRICT_NO_PYTHON=true
GO_PYTHON_HOT_PATH_OWNERSHIP_MODE=strict
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
ORCH_STORAGE_GOVERNANCE_MIN_FREE_GB=10
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
