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
- Run side-by-side recall/latency benchmarks against current Python path.
