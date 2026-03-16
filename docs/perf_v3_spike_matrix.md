# V3 Spike Backend Matrix (Rust Pivot)

Generated: 2026-03-16  
Project: `contextlattice`

## Run Artifacts (Latest)

- End-to-end matrix (all profiles): `bench/results/backend_lane_matrix_20260316T103732Z.json`
- Direct sidecar matrix (all spike backends): `bench/results/memory_bank_spike_direct_matrix_20260316T103252Z.json`
- External candidate probe: `bench/results/high_priority_candidate_probe_20260316T102234Z.json`

## End-to-End Matrix (Orchestrator Path, Latest)

| profile | avg p95 ms | delta vs baseline | note |
|---|---:|---:|---|
| baseline_qdrant_rollups | 109.093 | +0.000% | control lane |
| memory_bank_helixdb_spike | 59.812 | +45.173% | fallback path (adapter unconfigured) |
| memory_bank_trieve_spike | 73.757 | +32.391% | fallback path (adapter unconfigured) |
| rust_lane_usearch_tantivy | 224.298 | -105.603% | regression in latest run |
| memory_bank_lancedb_spike | 184.949 | -69.533% | improved vs prior run, still slower than baseline |
| memory_bank_tantivy_spike | 813.030 | -645.263% | heavy tail outliers |
| memory_bank_quickwit_spike | 1670.762 | -1431.502% | heavy tail outliers |
| memory_bank_meilisearch_spike | 3109.448 | -2750.273% | severe tail outliers |

Memory-bank backend delta for full run:

- attempts: `36`
- successes: `24`
- failures: `12`
- fallbacks: `12`

Interpretation:

- Profile-level p95 is still dominated by intermittent tail events on inline memory-bank spike lanes.
- Baseline (`qdrant + topic_rollups`) remains the most stable production lane in this sample.
- `trieve_spike` and `helixdb_spike` are not configured; observed behavior is fallback, not true backend performance.

## Direct Sidecar Matrix (Backend-Isolated, Latest)

| backend | avg p95 ms | avg result count | avg error rate | max p95 ms |
|---|---:|---:|---:|---:|
| quickwit_spike | 21.867 | 10.667 | 0.0 | 39.621 |
| tantivy_spike | 39.212 | 10.667 | 0.0 | 52.777 |
| meilisearch_spike | 50.240 | 5.667 | 0.0 | 65.103 |
| lancedb_spike | 57.044 | 9.444 | 0.0 | 64.457 |
| trieve_spike | 10.581 | 0.000 | 1.0 | 22.294 |
| helixdb_spike | 9.989 | 0.000 | 1.0 | 21.344 |

Interpretation:

- In isolation, `quickwit_spike` remains the fastest configured lexical lane in this run.
- `lancedb_spike` is functional and medium-latency in direct mode.
- Adapter readiness remains incomplete for `trieve_spike` and `helixdb_spike`.
- End-to-end degradation is integration-path overhead/tail behavior, not just direct backend speed.

## Current Keep/Use Guidance

- Keep production default on `qdrant + topic_rollups`.
- Keep memory-bank spike experimentation behind policy/profile controls.
- Prioritize quickwit-first tuning for memory-bank lexical enrichment.
- Do not promote trieve/helix lanes until real adapter endpoints are configured and verified.
