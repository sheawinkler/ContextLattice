# Ultra Recall / Ultra Performance DB Stack Recommendation (2026-03-16)

Project: `contextlattice`  
Goal: seamless UX with low-tail read latency and high recall quality across sources.

## Evidence Inputs

### Local measured runs

- End-to-end matrix: `bench/results/backend_lane_matrix_20260316T102223Z.json`
- Direct sidecar matrix: `bench/results/memory_bank_spike_direct_matrix_20260316T103252Z.json`
- Candidate probe: `bench/results/high_priority_candidate_probe_20260316T102234Z.json`

### External references (primary docs / benchmark frameworks)

- Qdrant hybrid queries: <https://qdrant.tech/documentation/concepts/hybrid-queries/>
- LanceDB full-text search: <https://lancedb.com/docs/search/full-text-search/>
- Weaviate hybrid search: <https://docs.weaviate.io/weaviate/search/hybrid>
- Milvus hybrid search: <https://milvus.io/docs/hybrid_search_with_milvus.md>
- OpenSearch vector search: <https://opensearch.org/docs/latest/vector-search/>
- Vespa hybrid tutorial: <https://docs.vespa.ai/en/tutorials/hybrid-search.html>
- ANN Benchmarks: <https://ann-benchmarks.com/>
- VectorDBBench: <https://github.com/zilliztech/VectorDBBench>

## What The Local Data Says Right Now

- Fastest direct lexical backend in latest full run: `quickwit_spike` (`avgP95Ms=21.867`, `avgResultCount=10.667`).
- Most stable end-to-end profile: `baseline_qdrant_rollups` (`avgP95Ms=139.565`).
- Rust lane (`usearch_ann + tantivy_lexical`) is close but slower in the latest run (`avgP95Ms=144.630`).
- `lancedb_spike` is now functional, but current end-to-end profile has heavy tails.
- `trieve_spike` and `helixdb_spike` are not configured; observed gains are fallback behavior.
- Direct spike backend p95 is still volatile run-to-run, so promotion should continue to use multi-run median + p95/p99 gates.

## Recommended DB Set (Now)

### 1) Primary semantic recall (ship lane)

- `qdrant` as primary vector/hybrid semantic store.
- Why: strongest current production stability in orchestrator path and mature hybrid controls.

### 2) Structured fast context lane

- `topic_rollups` as object-level summary index with raw-file backpointers.
- Why: consistently low latency and keeps deep read focused.

### 3) Lexical deep-detail accelerator (phase-in)

- `quickwit_spike` for memory-bank lexical acceleration.
- Why: best direct latency + coverage among currently integrated spike backends.
- Constraint: keep profile-gated until orchestrator tail behavior is flattened.

### 4) Memory durability / broad recall lane

- `memory_bank` native lane remains enabled with fail-open fallback.
- Why: preserves recall breadth and cache population, even if spike backends fail.

### 5) Slow-source quality lane

- Keep `letta` + `mindsdb` as deep lane only (not on fast-path critical budget).
- Why: potential quality value; currently too variable for strict low-latency SLA.

## Recommended Candidates To Test Next (Highest Value)

### A) High-value near-term

1. `milvus` (strong ANN + hybrid maturity, proven at larger scales).
2. `weaviate` (hybrid + ecosystem strength, useful if richer filtering semantics are needed).
3. `vespa` (best candidate for advanced ranking pipelines when willing to take on higher ops complexity).

### B) Keep as tactical/adjacent options

1. `opensearch` vector search (fits orgs already running OpenSearch).
2. `lancedb` (good local/embedded workflow, keep for local sidecar experimentation).
3. `pgvectorscale` (good fit if Postgres-first operational model is preferred).

## Short Decision

For seamless UX today, the best stack is:

1. `qdrant` + `topic_rollups` as production default
2. `quickwit_spike` as the next promoted acceleration candidate (profile-gated)
3. `memory_bank` native as durable fallback
4. `letta/mindsdb` in deep lane only until their latency variance is reduced

This gives the strongest measured balance of speed, recall quality, and operational safety with the current codebase.
