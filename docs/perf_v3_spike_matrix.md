# V3 Spike Backend Matrix (Rust Pivot)

Generated: 2026-03-16
Project: `contextlattice`

## Run Artifacts

- Previous orchestrator matrix (pre-fix): `bench/results/backend_lane_matrix_v3_rust_pivot_timeout10_bypasscache_20260316T073507Z.json`
- Previous direct matrix (pre-fix): `bench/results/memory_bank_spike_direct_matrix_tantivyfix_20260316T073551Z.json`
- Reworked orchestrator matrix (post-fix): `bench/results/backend_lane_matrix_v3_rust_pivot_reworked_20260316T0803Z.json`
- Reworked direct matrix (post-fix): `bench/results/memory_bank_spike_direct_matrix_v3_rust_pivot_20260316T0802Z.json`

## What Changed

- Rust sidecar now waits for Meilisearch task completion (`create index`, `settings`, `document upsert`).
- Meilisearch document IDs are normalized to valid key format (with deterministic hash suffix).
- Sidecar sync/index build paths are single-flight guarded.
- Direct matrix harness now supports warmups (`--warmups`) so cold starts do not dominate p95.
- Bench requests force real execution (`bypass_pathway_cache=true`) for fair lane comparison.

## End-to-End Matrix (Orchestrator + Sources, Post-Fix)

| profile | data store | index | search | avg p95 ms | attempts | success | fail | fallback |
|---|---|---|---|---:|---:|---:|---:|---:|
| baseline_qdrant_rollups | qdrant+topic_rollups | hnsw+partitioned-rollup | semantic+rollup-hybrid | 95.334 | 0 | 0 | 0 | 0 |
| rust_lane_usearch_tantivy | qdrant+topic_rollups+memory_bank(native) | usearch-ann+tantivy-lexical | hybrid-semantic-lexical | 213.195 | 0 | 0 | 0 | 0 |
| memory_bank_meilisearch_spike | memory_bank(meilisearch)+qdrant+topic_rollups | meilisearch+hnsw | hybrid-meili-semantic | 206.289 | 9 | 9 | 0 | 3 |
| memory_bank_quickwit_spike | memory_bank(quickwit_compat)+qdrant+topic_rollups | inverted-index+hnsw | hybrid-quickwit-compat-semantic | 238.510 | 9 | 9 | 0 | 0 |
| memory_bank_tantivy_spike | memory_bank(tantivy)+qdrant+topic_rollups | tantivy-inverted+hnsw | hybrid-tantivy-semantic | 300.827 | 9 | 9 | 0 | 0 |

Overall spike backend delta (post-fix run):

- attempts: `27`
- successes: `27`
- failures: `0`
- fallbacks: `3`

## Direct Sidecar Matrix (Backend-Isolated, Post-Fix)

| backend | avg p95 ms | avg result count | avg error rate | max p95 ms |
|---|---:|---:|---:|---:|
| meilisearch_spike | 8.612 | 4.667 | 0.0 | 9.836 |
| quickwit_spike | 10.759 | 9.667 | 0.0 | 15.252 |
| tantivy_spike | 10.990 | 9.667 | 0.0 | 13.244 |

## Fix Impact vs Earlier Runs

- Meilisearch moved from ingest errors / zero reliable hits to successful indexing and direct hits.
- Spike profile execution is now stable in both direct and orchestrator paths (no spike failures in the reworked matrix).
- End-to-end latency remains dominated by qdrant + fanout orchestration path, not sidecar direct search time.

## All Potential Source Snapshot (Deep Query)

From `POST /v1/retrieval/query-with-grounding` with sources `[qdrant, topic_rollups, memory_bank, letta, mindsdb, mongo_raw]`:

| source | returned count |
|---|---:|
| qdrant | 28 |
| topic_rollups | 4 |
| memory_bank | 2 |
| letta | 0 |
| mindsdb | 0 |
| mongo_raw | 0 |

This confirms the spike backend lane is healthy while non-spike external sources remain sparse for this query/sample.
