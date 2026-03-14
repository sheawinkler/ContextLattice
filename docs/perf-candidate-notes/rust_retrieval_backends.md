# Rust Retrieval Backends (Issue #72)

Status: integrated as feature-flagged scaffolding in `crates/context_retrieval`.

## Added Features

- `qdrant_remote` (crate: `qdrant-client`)
- `usearch_ann` (crate: `usearch`)
- `tantivy_lexical` (crate: `tantivy`)

## Why Feature-Flagged

- Keeps default runtime stable.
- Avoids forcing all users to run extra retrieval backends.
- Allows targeted benchmarking by backend before default cutover.

## Enable For Local Experiments

```bash
cd crates
cargo test -p context_retrieval --features "qdrant_remote usearch_ann tantivy_lexical"
```

## Next Integration Step

- Wire `HybridRetrievalIndex` backend selection through orchestrator runtime flags.
  - `ORCH_RUST_RETRIEVAL_VECTOR_BACKEND=auto|qdrant_remote|usearch_ann`
  - `ORCH_RUST_RETRIEVAL_LEXICAL_BACKEND=auto|none|tantivy_lexical`
  - `ORCH_RUST_RETRIEVAL_BACKEND_STRICT=true|false`
- Runtime request path now carries `backend_policy` and surfaces it in retrieval debug (`runtime.rust_backend_policy` and `source_policy.runtime_backend_policy`).
- Run side-by-side recall/latency benchmarks against current Python path.
