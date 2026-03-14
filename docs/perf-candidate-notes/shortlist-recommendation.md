# Performance Shortlist Recommendation (Issue #69)

## Artifacts
- `bench/results/perf_shortlist_matrix_20260312T191622Z.json`
- `bench/results/perf_shortlist_matrix_20260314T084608Z.json`
- `bench/results/qdrant_tuning_20260312T191656Z.json`
- `bench/results/qdrant_tuning_20260314T084551Z.json`

## Current status
- Adapter boundaries and feature flags are in place:
  - `services/orchestrator/runtime/adapters/base.py`
  - `services/orchestrator/runtime/adapters/registry.py`
- Runtime backend policy wiring is now in place:
  - `ORCH_RUST_RETRIEVAL_VECTOR_BACKEND`
  - `ORCH_RUST_RETRIEVAL_LEXICAL_BACKEND`
  - `ORCH_RUST_RETRIEVAL_BACKEND_STRICT`
  - request/debug propagation via `backend_policy`
- Candidate notes are documented for `fastembed-rs`, `EmbedAnything`, `edgevec`, `zvec`, and `swiftide`.
- Runtime remains default-safe (no candidate adapter enabled by default).

## Recommendation
1. Keep current runtime defaults while Letta and MindsDB tail errors are still being actively reduced.
2. Prioritize `fastembed-rs` as first adapter spike for embedding throughput once read-path stability remains green for 24h.
3. Keep `edgevec` and `zvec` as ANN benchmark references; evaluate only after Qdrant tuning track converges.
4. Keep `EmbedAnything` and `swiftide` in ingestion/pipeline design track rather than immediate hot-path replacement.

## Go/No-Go criteria (unchanged)
- Demonstrated p95 improvement >= 20% on representative workloads.
- Timeout/error non-regression (<= baseline + 0.5%).
- Recall-quality saved gates pass.
- Immediate env-flag rollback validated.
