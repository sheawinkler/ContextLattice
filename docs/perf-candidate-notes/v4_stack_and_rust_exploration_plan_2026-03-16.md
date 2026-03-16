# ContextLattice V4 Stack and Rust Exploration Plan (2026-03-16)

## Promoted V4 Runtime Stack

### Fast retrieval lane (sync)
- `topic_rollups`
- `qdrant`
- `memory_bank` with:
  - primary: `quickwit_spike`
  - fallback: `meilisearch_spike`
  - native fallback: enabled

### Deep retrieval lane (async/non-blocking)
- `mindsdb`
- `mongo_raw`
- `letta`

### Promoted additive memory-bank adapter backends (selectable)
- `icm_spike`
- `shodh_spike`
- `memvid_spike`
- `surrealdb_spike`
- existing: `lancedb_spike`, `trieve_spike`, `helixdb_spike`

These are first-class policy targets in Go gateway + Python orchestrator + Rust memory-bank sidecar.

## Why this stack

- Keeps the current strongest low-tail latency path (`quickwit_spike` / `meilisearch_spike`) in fast sync.
- Preserves high-recall deep sources without blocking the user response path.
- Adds explicit expansion slots for high-performing Rust candidates from replacement matrix work (`icm`, `shodh`, `memvid`, `surrealdb`) with no further API contract changes.

## Current policy defaults (promoted)

- `ORCH_MEMORY_BANK_SEARCH_BACKEND=quickwit_spike`
- `ORCH_MEMORY_BANK_SPIKE_FALLBACK_BACKEND=meilisearch_spike`
- `ORCH_MEMORY_BANK_SPIKE_EMPTY_RESULT_FALLBACK=true`
- `ORCH_MEMORY_BANK_SPIKE_FALLBACK_TO_NATIVE=true`
- `ORCH_RETRIEVAL_MEMORY_BANK_DEFAULT_ENABLED=true`
- `ORCH_RETRIEVAL_FAST_SOURCES=topic_rollups,qdrant,memory_bank`
- `ORCH_RETRIEVAL_SLOW_SOURCES=mindsdb,mongo_raw,letta`

## Background Rust Exploration Plan

### Track A — Adapter activation (near-term)
1. Wire live endpoints for:
   - `icm_spike`
   - `shodh_spike`
   - `memvid_spike`
   - `surrealdb_spike`
2. Run `memory_bank_spike_direct_matrix.py` with all backends.
3. Run `backend_lane_matrix.py` with orchestrator lane profiles.
4. Promote only when:
   - `avgErrorRate == 0.0`
   - `avgP95Ms` improves vs current promoted lane or delivers materially higher recall coverage.

### Track B — Structured-memory quality (mid-term)
1. Use `surrealdb_spike` for trajectory and relationship-heavy recall slices.
2. Keep lexical engines for speed; use structured lane for precision filters.
3. Add recall gate checks after each promotion candidate run.

### Track C — Rust-first replacement progression (ongoing)
1. Continue evaluating replacements for the slowest deep sources first.
2. Prefer candidates with:
   - deterministic local benchmarking
   - low ops overhead
   - clean rollback path
3. Keep fall-open continuation active until replacement quality and stability are both verified.

## Verification checklist for each future promotion

1. Direct backend matrix (`p50/p95/error_rate/hit_rate`)
2. End-to-end orchestrator matrix (same metrics + timeout behavior)
3. Recall quality checks and saved recall gate
4. Runtime health + degraded mode validation
5. Rollback test
