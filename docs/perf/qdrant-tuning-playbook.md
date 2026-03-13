# Qdrant Tuning Playbook

## Scope
Reference-only tuning track for the existing Qdrant deployment. This is not a migration to a new vector database.

## Baseline Capture
1. Run:
```bash
gmake bench-qdrant-tuning
```
2. Archive output JSON under `bench/results/qdrant_tuning_<timestamp>.json`.
3. Record:
- `p50/p95/p99` latency
- timeout and error counts
- request throughput under fast/balanced/deep modes

Latest sample artifact:
- `bench/results/qdrant_tuning_20260312T191656Z.json`

## Tuning Matrix
Evaluate one profile change at a time:
1. Search/runtime profile:
- `ORCH_QDRANT_SEARCH_HNSW_EF`
- `ORCH_QDRANT_SEARCH_MODE_HNSW_EF`
- `ORCH_QDRANT_SEARCH_MODE_LIMIT_CAPS`
- `ORCH_QDRANT_SEARCH_EXACT`
- `ORCH_QDRANT_SEARCH_INDEXED_ONLY`
- `ORCH_QDRANT_SEARCH_TIMEOUT_RETRY_ENABLED`
- `ORCH_QDRANT_SEARCH_TIMEOUT_RETRY_LIMIT_FACTOR`
2. Index build profile:
- `QDRANT_HNSW_M`
- `QDRANT_HNSW_EF_CONSTRUCT`
3. Optimizer/compaction profile:
- `QDRANT_ALWAYS_RAM`
- retention/snapshot schedule impact

For each profile:
1. Capture env snapshot.
2. Run `gmake bench-qdrant-tuning`.
3. Compare against baseline.
4. Keep only if deep-read p95/p99 improves with no recall regression.

Recommended starter profile for mixed workloads:
- `ORCH_QDRANT_SEARCH_MODE_HNSW_EF={"fast":48,"balanced":96,"deep":128}`
- `ORCH_QDRANT_SEARCH_MODE_LIMIT_CAPS={"fast":80,"balanced":120,"deep":180}`

## Rollout
1. Canary on non-critical workloads.
2. Watch:
- `/telemetry/retrieval`
- `/telemetry/retrieval/source-quality`
- `/ops/queue/status`
3. Promote only after stable 24h window.

## Rollback
1. Restore previous env snapshot.
2. Restart orchestrator stack.
3. Re-run `gmake bench-qdrant-tuning` to confirm baseline restoration.
