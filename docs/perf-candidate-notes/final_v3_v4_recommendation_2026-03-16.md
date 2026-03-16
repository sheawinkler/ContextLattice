# Final V3/V4 Recommendation (2026-03-16)

Project: `contextlattice`  
Objective: maximize recall quality while reaching seamless read UX.

## Evidence Used

- Direct spike matrix (populated corpus): `bench/results/memory_bank_spike_direct_matrix_20260316T180944Z.json`
- End-to-end lane matrix (seeded corpus): `bench/results/backend_lane_matrix_20260316T181019Z.json`
- Adapter probe: `bench/results/high_priority_candidate_probe_20260316T181026Z.json`
- Additional populated external smokes (this run):
  - `milvus` (5000 vectors, HNSW): `p50_ms=8.12`, `p95_ms=20.025`, `avg_hits=10`
  - `weaviate` (2000 docs, BM25): `p50_ms=17.382`, `p95_ms=35.711`, `avg_hits=10`
  - `postgres_pgvector` (5000 vectors, ivfflat): `p50_ms=4.015`, `p95_ms=11.943`, `avg_hits=10`

## Current Matrix Summary

### Direct spike lane (memory-bank backend only)

| backend | avgP95Ms | avgErrorRate | avgResultCount |
|---|---:|---:|---:|
| `quickwit_spike` | 4.389 | 0.0 | 10.667 |
| `meilisearch_spike` | 8.500 | 0.0 | 5.667 |
| `tantivy_spike` | 15.831 | 0.0 | 10.667 |
| `lancedb_spike` | 19.695 | 0.0 | 7.278 |
| `helixdb_spike` | 143.108 | 0.0 | 10.667 |
| `trieve_spike` | 147.666 | 0.0 | 10.667 |

### End-to-end orchestrator lane

| profile | avgP95Ms | avgErrorRate |
|---|---:|---:|
| `memory_bank_meilisearch_spike` | 98.779 | 0.0 |
| `rust_lane_usearch_tantivy` | 115.150 | 0.0 |
| `memory_bank_quickwit_spike` | 115.979 | 0.0 |
| `memory_bank_tantivy_spike` | 137.243 | 0.0 |
| `baseline_qdrant_rollups` | 144.939 | 0.0 |
| `memory_bank_helixdb_spike` | 175.679 | 0.0 |
| `memory_bank_trieve_spike` | 179.998 | 0.0 |
| `memory_bank_lancedb_spike` | 186.160 | 0.0 |

## Meilisearch Clarification

Meilisearch now **does make the cut**. Earlier outlier failures were traced to Docker VM storage pressure (`os error 28`, no space left on device), not intrinsic retrieval quality. After cleanup and reruns, Meili is:

- low-latency in direct lane
- best memory-bank spike lane in end-to-end latency
- still lower `avgResultCount` than quickwit/tantivy in direct lane, so recall coverage gating remains required

## Native Trieve / Helix Findings

### Trieve (native)

- Native Trieve is not a drop-in single service in our current shape.
- Local `docker-compose.yml` requires a larger stack (Postgres, Redis, Qdrant, MinIO, Tika, Keycloak, server, workers/frontends).
- Integration cost is medium-high; likely worthwhile only if we want Trieve-native reranking and full API semantics.

### Helix (native)

- Helix is query/schema-driven (HelixQL) and deploys compiled query endpoints (`helix push dev`).
- This is not a direct `/search` replacement; it is more of a data model + query platform migration.
- Integration cost is high but strategically interesting for graph+vector convergence.

## MindsDB Alternatives (Postgres-like)

MindsDB remains useful for deep/slow SQL-assisted reasoning, but for latency-sensitive retrieval:

- `postgres + pgvector` (optionally `pgvectorscale`) is a strong alternative and showed very strong p50/p95 in populated smoke.
- It is operationally simpler than MindsDB for raw retrieval and easier to benchmark deterministically.
- Tradeoff: MindsDB offers higher-level AI-SQL workflows; pgvector stack requires explicit pipeline ownership.

## Final Recommendation

## V3 (now)

1. Keep `qdrant + topic_rollups` as primary production lane for balanced recall/quality.
2. Promote `memory_bank_meilisearch_spike` to first lexical spike candidate with recall guardrails.
3. Keep `memory_bank_quickwit_spike` as high-recall lexical fallback (higher result count profile).
4. Keep `memory_bank_tantivy_spike` as secondary fallback lane.
5. Keep `letta` and `mindsdb` as deep-mode asynchronous sources only (not sync fast-path blockers).

## V4 (replacement/pivot lane if needed)

1. Pilot `postgres + pgvector (+pgvectorscale)` as a structured deep-lane replacement candidate for parts of MindsDB duties.
2. Run native Trieve POC only if we want Trieve-native ranking/recommendation semantics beyond adapter mode.
3. Run native Helix POC only as a deliberate graph+vector architecture pivot (not incremental replacement).
4. Preserve telemetry isolation (telemetry store only), keep retrieval corpora clean (`memory_bank`, `qdrant`, rollups).

## Bottom Line

- We can keep the sophisticated multi-source architecture.
- Meilisearch should be promoted (with recall gates), not excluded.
- MindsDB should stay deep-lane for now; pgvector stack is the most practical performance-oriented Postgres-like alternative to evaluate as a replacement candidate.
