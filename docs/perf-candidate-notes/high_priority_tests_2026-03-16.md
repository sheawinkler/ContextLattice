# High-Priority Candidate Test Round (2026-03-16)

Artifacts:
- `bench/results/backend_lane_matrix_20260316T090443Z_seedfix.json`
- `bench/results/memory_bank_spike_direct_matrix_20260316T090659Z_priority_round.json`
- `bench/results/high_priority_candidate_probe_20260316T090823Z.json`
- `bench/results/backend_lane_matrix_20260316T093119Z_external_adapters_live.json`
- `bench/results/memory_bank_spike_direct_matrix_20260316T093119Z_extended_adapters_live.json`
- `bench/results/high_priority_candidate_probe_20260316T093141Z_post_adapter_integration.json`

## Recall Coverage Fix Validation

- Seed corpus writes: attempted `3`, succeeded `3`, verified `True` (verification results `3`).
- Result counts in seeded backend lane matrix were non-zero across all tested cases/profiles.

## Backend Lane (seeded)

| Profile | avg p95 (ms) | delta vs baseline | avg error rate |
|---|---:|---:|---:|
| `baseline_qdrant_rollups` | `61.033` | `0.000%` | `0.000` |
| `rust_lane_usearch_tantivy` | `135.114` | `-121.379%` | `0.000` |
| `memory_bank_quickwit_spike` | `335.764` | `-450.135%` | `0.000` |
| `memory_bank_tantivy_spike` | `296.660` | `-386.065%` | `0.000` |
| `memory_bank_meilisearch_spike` | `3182.194` | `-5113.891%` | `0.000` |

## Direct Spike Backend (sidecar)

| Backend | avg p95 (ms) | max p95 (ms) | avg result count | avg error rate |
|---|---:|---:|---:|---:|
| `quickwit_spike` | `15.998` | `25.604` | `3.000` | `0.000` |
| `tantivy_spike` | `56.641` | `87.444` | `2.667` | `0.000` |
| `meilisearch_spike` | `27.558` | `39.960` | `1.667` | `0.000` |

## External Priority Probes

| Candidate | Status | URL |
|---|---|---|
| `lancedb` | `skipped_unconfigured` | `-` |
| `trieve` | `skipped_unconfigured` | `-` |
| `helixdb` | `skipped_unconfigured` | `-` |

## Adapter Integration Pass (Rust sidecar + orchestrator/go policy)

- Rust sidecar now exposes backend modes:
  - `tantivy_spike`, `quickwit_spike`, `meilisearch_spike`
  - `lancedb_spike`, `trieve_spike`, `helixdb_spike`
- Adapter endpoints are currently unconfigured in `.env`:
  - sidecar `/health` reports `external_backends=false` for all three adapter lanes
  - direct calls return explicit `502 ... endpoint is not configured`
- Orchestrator + gateway now preserve and propagate these backend policy values end-to-end.
- Backend lane matrix confirms explicit fail-open behavior when adapter lanes are requested:
  - each adapter profile recorded `attempts=3`, `failures=3`, `fallbacks=3`, `successes=0`
  - retrieval still completed via native fallback path.

## Next Actions

1. Keep seeded corpus validation enabled in benchmark matrix to prevent false sparse-hit regressions.
2. Promote quickwit/tantivy direct-spike lane experiments first (best p95 among currently integrated spike backends).
3. Wire at least one external candidate endpoint (`trieve` or `helixdb`) and rerun both:
   - `tooling/python/bench/high_priority_candidate_probe.py`
   - `tooling/python/bench/memory_bank_spike_direct_matrix.py --backends ...`
4. Promote an adapter lane only after it shows non-zero results with error-rate <= existing spike lanes and lower p95 under cache-busted runs.
