# Rust Crates (Migration Scaffolds)

These crates back Phases 2-4 of the migration plan.

- `context_codec`: state serialization + checksums
- `context_engine`: memory graph primitives
- `context_retrieval`: retrieval ranking/index primitives

Run checks:

```bash
cargo test --manifest-path crates/Cargo.toml
```
