# Backend Lane Matrix Run (2026-03-16)

Artifact:
- `bench/results/backend_lane_matrix_20260316T_live.json`

Command:

```bash
python3 tooling/python/bench/backend_lane_matrix.py \
  --base-url http://127.0.0.1:8075 \
  --api-key "$API_KEY" \
  --project contextlattice \
  --runs 3 \
  --timeout 90 \
  --output bench/results/backend_lane_matrix_20260316T_live.json
```

## Summary

| Profile | avg p95 (ms) | delta vs baseline | avg error rate |
|---|---:|---:|---:|
| `baseline_qdrant_rollups` | `232.362` | `0.000%` | `0.0` |
| `rust_lane_usearch_tantivy` | `201.378` | `+13.334%` | `0.0` |
| `memory_bank_tantivy_spike` | `207.062` | `+10.888%` | `0.0` |
| `memory_bank_quickwit_spike` | `223.393` | `+3.860%` | `0.0` |
| `memory_bank_meilisearch_spike` | `250.980` | `-8.012%` | `0.0` |

## Observations

1. Rust lane request (`usearch_ann + tantivy_lexical`) was the best performer in this run.
2. All lexical spike profiles were fallback-only tests because `ORCH_MEMORY_BANK_SPIKE_HTTP_URL` is not configured.
3. Telemetry delta confirms fallback-only behavior:
   - memory-bank spike attempts delta: `0`
   - memory-bank spike successes delta: `0`
   - memory-bank spike fallbacks delta: `27`
4. Letta remains untouched in this test and stayed out of the sync critical path.

## Recommendation

1. Keep Letta in the current async/archival role.
2. Proceed with the Rust backend lane as the primary direction for Phase A/1 continuation.
3. For meaningful meilisearch/quickwit/tantivy comparisons, wire a real spike sidecar URL first, then re-run this matrix.

## Follow-up Runs (same day)

Artifacts:
- `bench/results/backend_lane_matrix_20260316T081022Z.json`
- `bench/results/backend_lane_matrix_20260316T083942Z_post_tuning.json`
- `bench/results/backend_lane_matrix_20260316T084531Z_gatewaygo_post_tuning.json`

| Artifact | Base URL | Baseline avg p95 (ms) | Rust lane avg p95 (ms) | Rust delta vs baseline |
|---|---|---:|---:|---:|
| `20260316T081022Z` | `:8075` | `58.861` | `45.144` | `+23.304%` |
| `20260316T083942Z_post_tuning` | `:8075` | `54.048` | `97.547` | `-80.482%` |
| `20260316T084531Z_gatewaygo_post_tuning` | `:8091` | `37.353` | `75.122` | `-101.114%` |

Notes:
1. `:8091` run validates the updated gateway-go logic path; `:8075` runs validate current default ingress path.
2. All three runs still show sparse/zero recall counts for these benchmark queries, so latency-only deltas are unstable and quality/coverage gates are the next priority.
3. Memory-bank spike profiles on `:8091` were fallback-only because a spike sidecar URL was not configured in that process environment.
