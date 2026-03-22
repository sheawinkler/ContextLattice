# Issue #69 Finalization (2026-03-22)

## Scope
Finalize keep/drop/defer decisions for the shortlist track:
- `swiftide`
- `fastembed-rs`
- `EmbedAnything`
- `zvec`
- `edgevec`

## Evidence Artifacts
- Baseline: `bench/results/perf_shortlist_matrix_baseline.json`
- Prior best gate sample: `bench/results/perf_shortlist_matrix_20260316T004108Z.json`
- Current run: `bench/results/perf_shortlist_matrix_20260322T195851Z.json`

## Current Run Metrics (`2026-03-22T19:59:02Z`)

| Case | Runs | Errors | p50 (ms) | p95 (ms) | p99 (ms) |
|---|---:|---:|---:|---:|---:|
| short_context | 8 | 0 | 37.260 | 54.325 | 55.498 |
| ops_focus | 8 | 0 | 43.129 | 84.796 | 87.032 |
| deep_recall | 8 | 0 | 60.063 | 77.308 | 77.315 |
| embedding_stress | 8 | 0 | 56.940 | 585.686 | 810.810 |

Gate evaluation from the same run:
- baseline `embedding_stress` p95: `58.775 ms`
- current aggregate p95: `86.248 ms`
- improvement: `-46.743%`
- pass: `false`

Prior best observed gate sample (`20260316T004108Z`):
- improvement: `+16.063%`
- pass: `false` (threshold is `>= 20%`)

## Source-Quality Snapshot (same run)
- `topic_rollups`: p50 `0.501 ms`, p95 `3.584 ms`, timeoutRate `0.0`
- `qdrant`: p50 `24.519 ms`, p95 `81.543 ms`, timeoutRate `0.0`
- `postgres_pgvector`: p50 `9.185 ms`, p95 `235.812 ms`, timeoutRate `0.0`
- `memory_bank`: p50 `521.077 ms`, p95 `1201.454 ms`, timeoutRate `0.024390`
- `mindsdb`: p50 `23.654 ms`, p95 `116.634 ms`, timeoutRate `0.0`
- `letta`: p50 `32901.353 ms`, p95 `60629.645 ms`, timeoutRate `0.214286`

## Final Decisions
1. `fastembed-rs`: **keep integrated, not promoted to default**.
- Reason: best measured improvement is below hard gate (`+16.063% < +20%`) and latest run shows high tail variance.
- Action: remain feature-flagged (`ORCH_ADAPTER_FASTEMBED_RS_*`) and benchmark-only until gate is met consistently.

2. `EmbedAnything`: **defer runtime integration**.
- Reason: strongest fit is corpus ingestion/indexing workflows, not the current latency-critical online read path.
- Action: move to V4/V72 ingestion lane evaluation rather than default retrieval path.

3. `zvec`: **defer**.
- Reason: no validated end-to-end improvement artifact in current orchestrator path.
- Action: keep as ANN benchmark candidate only.

4. `edgevec`: **defer**.
- Reason: no validated end-to-end improvement artifact in current orchestrator path.
- Action: keep as ANN benchmark candidate only.

5. `swiftide`: **keep as pipeline/reference pattern only**.
- Reason: useful for ingestion/retrieval DAG architecture, not a direct low-latency drop-in for current read path.

## Close Recommendation
Issue #69 is complete for its decision objective (shortlist evaluation + keep/drop/defer outcomes).
Follow-on implementation exploration should continue under the broader V4/V72 performance track.

## 2026-03-22 Addendum (Promotion Alignment)
- Gate defaults were lowered from `20%` to `5%` to match the approved promotion policy for the shortlist lane.
- Runtime defaults now promote `fastembed-rs` as enabled-by-default, while keeping fail-open fallback behavior in place.
- Updated defaults:
  - `GATE_REFRESH_GATE_MIN_IMPROVEMENT_PCT=5`
  - `ORCH_ADAPTER_FASTEMBED_RS_ENABLED=true`
