# Memory Bank Replacement Spike (Non-ANE)

This spike starts a pluggable backend lane for `memory_bank` lexical reads while keeping Qdrant as the primary vector store.

## Runtime flags

```bash
# Keep memory_bank out of default blocking retrieval path
ORCH_RETRIEVAL_MEMORY_BANK_DEFAULT_ENABLED=false

# Backend mode: native | disabled | meilisearch_spike | quickwit_spike | tantivy_spike | lancedb_spike | trieve_spike | helixdb_spike
ORCH_MEMORY_BANK_SEARCH_BACKEND=native

# Optional spike sidecar contract
ORCH_MEMORY_BANK_SPIKE_HTTP_URL=
ORCH_MEMORY_BANK_SPIKE_SEARCH_ROUTE=/search
ORCH_MEMORY_BANK_SPIKE_TIMEOUT_SECS=1.5
ORCH_MEMORY_BANK_SPIKE_FALLBACK_TO_NATIVE=true

# Optional external adapter endpoints consumed by memory-bank-spike-rs
MEMORY_BANK_SPIKE_RS_EXTERNAL_TIMEOUT_SECS=12
MEMORY_BANK_SPIKE_RS_LANCEDB_URL=
MEMORY_BANK_SPIKE_RS_LANCEDB_SEARCH_ROUTE=/search
MEMORY_BANK_SPIKE_RS_LANCEDB_API_KEY=
MEMORY_BANK_SPIKE_RS_TRIEVE_URL=
MEMORY_BANK_SPIKE_RS_TRIEVE_SEARCH_ROUTE=/search
MEMORY_BANK_SPIKE_RS_TRIEVE_API_KEY=
MEMORY_BANK_SPIKE_RS_HELIXDB_URL=
MEMORY_BANK_SPIKE_RS_HELIXDB_SEARCH_ROUTE=/search
MEMORY_BANK_SPIKE_RS_HELIXDB_API_KEY=
```

## Sidecar request/response contract

### Request

```json
{
  "query": "profitability baseline ladder",
  "limit": 10,
  "project": "contextlattice",
  "topic_path": "runbooks/profitability",
  "backend": "meilisearch_spike"
}
```

### Response

```json
{
  "results": [
    {
      "project": "contextlattice",
      "file": "runbooks/profitability/baseline_ladder.md",
      "summary": "...",
      "score": 0.91,
      "topic_path": "runbooks/profitability"
    }
  ]
}
```

## Evaluation sequence

1. `native` baseline: run `tooling/python/bench/perf_shortlist_matrix.py`.
2. Enable `meilisearch_spike` sidecar and rerun matrix.
3. Enable `quickwit_spike` sidecar and rerun matrix.
4. Enable `tantivy_spike` sidecar and rerun matrix.
5. Configure `lancedb_spike` endpoint and rerun matrix.
6. Configure `trieve_spike` endpoint and rerun matrix.
7. Configure `helixdb_spike` endpoint and rerun matrix.
8. Compare `p50/p95/p99`, timeout rate, and recall quality; keep only winning path.

## Acceptance gate

- No regression in recall quality saved eval cases.
- `p95` latency improvement >= 20% for memory-bank lane.
- Timeout rate non-regression (or lower) for balanced/deep read profiles.
- Fallback to `native` verified under sidecar failure.
