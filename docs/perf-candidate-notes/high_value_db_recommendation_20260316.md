# High-Value DB Spike Results and Recommendations (2026-03-16)

## Scope

Objective:
- configure `trieve_spike` and `helixdb_spike` for real, non-error responses
- benchmark against existing spike backends
- test additional high-value DB candidates for next phase

Artifacts:
- `bench/results/memory_bank_spike_direct_matrix_latest.json`
- `bench/results/backend_lane_matrix_latest.json`
- comparison baselines:
  - `bench/results/memory_bank_spike_direct_matrix_20260316T165238Z.json`
  - `bench/results/backend_lane_matrix_20260316T165434Z.json`

## Configuration Outcome

Implemented:
- `services/external_spike_adapter` (shared adapter image for `trieve_spike` and `helixdb_spike`)
- compose wiring in:
  - `docker-compose.yml`
  - `docker-compose.lite.yml`
- default URL wiring in `.env.example`

Health:
- `http://127.0.0.1:8098/health` (`trieve_spike`): ok, docs loaded, no error
- `http://127.0.0.1:8099/health` (`helixdb_spike`): ok, docs loaded, no error
- `http://127.0.0.1:8096/health`: both external backends configured

Note:
- `trieve_spike` and `helixdb_spike` are compatibility adapters over memory-bank files.
- They provide real retrieval results for fair lane testing, but are not native vendor engines yet.

## Direct Spike Matrix (current)

Source: `memory_bank_spike_direct_matrix_latest.json`

| backend | avgP95Ms | avgErrorRate | avgResultCount |
|---|---:|---:|---:|
| `quickwit_spike` | 2.168 | 0.0 | 10.667 |
| `meilisearch_spike` | 5.625 | 0.0 | 5.667 |
| `tantivy_spike` | 15.553 | 0.0 | 10.667 |
| `lancedb_spike` | 51.193 | 0.0 | 6.5 |
| `trieve_spike` | 135.931 | 0.0 | 10.667 |
| `helixdb_spike` | 141.657 | 0.0 | 10.667 |

Delta vs earlier run (`20260316T165238Z`):
- `quickwit_spike`: 20.435 -> 2.168 (89.39% faster)
- `meilisearch_spike`: 6767.618 -> 5.625 (99.92% faster)
- `trieve_spike`: 177.772 -> 135.931 (23.54% faster)
- `helixdb_spike`: 235.593 -> 141.657 (39.87% faster)

## End-to-End Lane Matrix (current)

Source: `backend_lane_matrix_latest.json`

| profile | avgP95Ms | avgErrorRate |
|---|---:|---:|
| `baseline_qdrant_rollups` | 35.05 | 0.0 |
| `memory_bank_quickwit_spike` | 50.827 | 0.0 |
| `rust_lane_usearch_tantivy` | 54.224 | 0.0 |
| `memory_bank_tantivy_spike` | 60.173 | 0.0 |
| `memory_bank_meilisearch_spike` | 71.82 | 0.0 |
| `memory_bank_lancedb_spike` | 101.537 | 0.0 |
| `memory_bank_helixdb_spike` | 154.715 | 0.0 |
| `memory_bank_trieve_spike` | 156.44 | 0.0 |

Delta vs earlier run (`20260316T165434Z`):
- `baseline_qdrant_rollups`: 155.023 -> 35.05 (77.39% faster)
- `memory_bank_quickwit_spike`: 2148.423 -> 50.827 (97.63% faster)
- `memory_bank_tantivy_spike`: 1080.466 -> 60.173 (94.43% faster)
- `memory_bank_meilisearch_spike`: 11234.171 -> 71.82 (99.36% faster)
- `memory_bank_lancedb_spike`: 1348.172 -> 101.537 (92.47% faster)
- `memory_bank_trieve_spike`: 1171.292 -> 156.44 (86.64% faster)
- `memory_bank_helixdb_spike`: 848.713 -> 154.715 (81.77% faster)

## Additional High-Value Candidate Smokes

- Weaviate (v1.36.2, BM25 smoke): `latency_ms=42.804`, `hits=5`
- Milvus (v2.6.2, vector search smoke): `latency_ms=5.601`, `hits=0` in this quick script path (needs proper index/load tuning harness before comparison use)
- Vespa (container readiness smoke): did not reach healthy state within test window in this environment

## Recommendation (Current)

For production lane now:
1. Keep `qdrant + topic_rollups` as primary default for best blend of speed and recall coverage.
2. Keep `memory_bank_quickwit_spike` as the first lexical enrichment lane.
3. Keep `memory_bank_tantivy_spike` as secondary lexical fallback.
4. Keep `trieve_spike` and `helixdb_spike` enabled for validation and cache warming, but not in default fast path.

For next test cycle:
1. Build native Trieve and native Helix integrations before any promotion decision.
2. Add a proper Milvus harness (index creation params + persisted collection + repeatable hit checks) before ranking it.
3. Keep Vespa as a deferred candidate due startup/operational overhead in local developer loops.

## Memory Quality and Disk Considerations

- Memory quality (trajectory fidelity):
  - strongest when combining object rollups + raw ID pointers + qdrant semantic recall
  - lexical spikes should enrich only when they add non-duplicate grounded passages
- Disk:
  - telemetry narrowing materially reduced growth pressure
  - keep telemetry out of memory-bank/qdrant/mindsdb and in dedicated telemetry store only
  - treat memory-bank as knowledge corpus, not event stream
