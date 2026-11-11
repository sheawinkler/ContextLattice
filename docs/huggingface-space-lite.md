# Hugging Face Docker Space (Lite, Go-native)

This guide deploys the single-container lite lane on Hugging Face Spaces using the Go gateway runtime.

## Resource profile (hf-lite lane)

- Target lane: current public lite profile.
- Baseline sizing: `2-4` vCPU, `4-8 GB` RAM, `20-50 GB` SSD.
- Recommended disk headroom: keep at least `10 GB` free in `/data` to avoid compaction/rewrite pressure.
- Retrieval default in this lane: `topic_rollups` (single-container-safe).

## What this repo now provides

- `Dockerfile.hf-lite` builds `services/gateway-go` and runs it directly on port `7860`.
- Runtime defaults for strict Go ownership and sqlite-safe lite mode.
- Native `linux/amd64` or `linux/arm64` builds, a bundled response-contract registry, and a `/readyz` health check.
- Authoritative memory and gateway state under `/data`, alongside the SQLite stores.
- `.dockerignore` to keep build context small and deterministic.

## Space setup (manual UI steps)

1. Create a new Hugging Face Space.
2. Choose SDK: `Docker`.
3. Push this repository branch to the Space repo.
4. Hugging Face expects a root `Dockerfile`:
   - copy `Dockerfile.hf-lite` to `Dockerfile` in the Space repo before build.
5. Attach persistent storage mounted at `/data`. Without it, replacing a container can lose memory, not just a cache.
6. In Space variables, set:
   - `PORT=7860`
7. For a network-accessible deployment, enable the strict posture below and set this Space secret:
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
GO_RETRIEVAL_NATIVE_QDRANT_ENABLED=false
GO_WRITE_QDRANT_FANOUT_MODE=disabled
ORCH_RETRIEVAL_SOURCES=topic_rollups
ORCH_RETRIEVAL_DEFAULT_SOURCES=topic_rollups
ORCH_RETRIEVAL_FAST_SOURCES=topic_rollups
TOPIC_ROLLUP_SQLITE_ENABLED=true
TOPIC_ROLLUP_SQLITE_FTS_ENABLED=true
TOPIC_ROLLUP_SQLITE_VEC_ENABLED=true
TOPIC_ROLLUP_SQLITE_PATH=/data/topic_rollups.sqlite3
TASK_DB_PATH=/data/agent_tasks.db
GO_MEMORY_STORE_ROOT=/data/memory
MEMORY_BANK_ROOT=/data/memory
CONTEXTLATTICE_GATEWAY_STATE_ROOT=/data/state/gateway
CONTEXTLATTICE_AGENT_CONTRACTS_PATH=/app/config/agent_contracts/agent_output_contracts.json
FANOUT_OUTBOX_PAYLOAD_BLOB_DIR=/data/fanout_payload_blobs
ORCH_STORAGE_GOVERNANCE_DISK_ROOT=/data
ORCH_STORAGE_GOVERNANCE_ENABLED=true
ORCH_STORAGE_GOVERNANCE_RUN_ON_STARTUP=true
ORCH_STORAGE_GOVERNANCE_MIN_FREE_GB=10
SIGNAL_REFRESH_ENABLED=false
OVERRIDE_REFRESH_ENABLED=false
SINK_RETENTION_ENABLED=false
```

Strict production posture (required before exposing a deployment to untrusted clients):

```env
CONTEXTLATTICE_ENV=strict
ORCH_SECURITY_STRICT=true
ORCH_PRODUCTION_REQUIRE_API_KEY=true
CONTEXTLATTICE_ORCHESTRATOR_API_KEY=<secret>
```

## Drift controls

- Mount persistent storage at `/data` for memory, history, task, and gateway-state continuity. Persisting only the SQLite rollup index is insufficient.
- Do not replace or remove an older container until its authoritative memory and state have been backed up and read back successfully.
- If you want maximum determinism, disable learning adaptation:
  - `LEARNING_LOOP_ENABLED=false`
- Trigger a manual compaction/governance run when needed:
  - `POST /maintenance/storage/run`
- Inspect storage state:
  - `GET /telemetry/storage`

## Local verification before push

```sh
docker build --platform linux/amd64 -f Dockerfile.hf-lite -t contextlattice-hf-lite .
docker volume create contextlattice-lite-data
docker run --rm -p 127.0.0.1:7860:7860 \
  --mount source=contextlattice-lite-data,target=/data contextlattice-hf-lite
curl --fail http://127.0.0.1:7860/readyz
```

Use `--platform linux/arm64` for an ARM64 host. `/health` proves liveness, not readiness or persistence.

The repository acceptance helper uses only a disposable container and a new data directory. It writes memory, a claim and a queued task, validates contracted responses and the raw search response separately, replaces the container, then verifies exact content, history event digests, fixture-bound search, claim and task continuity. It creates an ephemeral gateway API key for the authenticated owner-local task surface, even when generic lite security is relaxed. It never executes the task, connects to the normal runtime, or deletes an existing volume:

```sh
python3 scripts/hf_lite_acceptance.py --image contextlattice-hf-lite \
  --docker-context default --artifact-dir ./artifacts/hf-lite-replacement
```

Use a fresh artifact directory for each run. A receipt proves same-image replacement only unless `--upgrade-image` names a different image; that mode also tests forward replacement and rollback against the same disposable data. This is a developer/release test, not a Python runtime dependency.

Use an image built from the public lane for this owner-local fixture. Paid shared-workspace task authority needs its own authenticated, entitled fixture; a public API key or a private development opt-in does not prove paid authorization. Queued-task persistence is not proof of task execution or independent review.

For source-bound release evidence, build with `CONTEXTLATTICE_SOURCE_COMMIT` and `CONTEXTLATTICE_SOURCE_TREE` build arguments, then supply the matching `--expected-source-commit` and `--expected-source-tree` to the helper. These refer to the upgrade image when one is supplied. Every phase checks that the running binary's build identity agrees with its image labels. Default `unknown` build identities can only produce an unbound development smoke, not source-bound release evidence. This fixture gate does not replace edition/authentication, real-data migration or the other release checks.

## Migrating an older lite container

Older images defaulted authoritative memory outside `/data`. Mounting a data volume on a replacement does not recover those files automatically.

1. Stop writes and record the old image identity, environment overrides, storage mounts, and a known memory file's digest.
2. Back up `/data` and the **resolved** old memory root and gateway-state root before deleting the old container. With no override, memory was typically at `/tmp/contextlattice-memory-bank`; gateway state used its own default. Inspect the actual container rather than assuming either path.
3. With both runtimes stopped, restore those roots into `memory/` and `state/gateway/` on the persistent data volume. Preserve ownership and the original backups; do not overwrite a populated destination blindly.
4. Start the new image with that volume, require `/readyz`, and compare exact memory content and history with the backup. Confirm search and response-contract validation separately.
5. If verification fails, stop the new runtime and restore the old image **with the preserved original data**. A newer runtime may change state; switching the image alone is not a data rollback.

The disposable acceptance test does not replace a backup or prove migration of an operator's existing data.
