# Orchestrator Migration Runtime

This package provides the Phase 1 interface boundary and Phase 2-8 adapters.

## Modules

- `interfaces.py`: stable contracts (`Codec`, `MemoryStore`, `Retriever`, `Scheduler`, `StateDelta`)
- `flags.py`: feature flags and mode toggles
- `python_impl.py`: Python baseline implementations
- `rust_stub.py`: Rust codec/memory/retrieval proxies with Python fallback
- `go_scheduler.py`: Go scheduler proxy with Python fallback
- `registry.py`: runtime composition and health snapshot

## App Integration

`services/orchestrator/app.py` initializes this runtime lazily and routes:

- memory search + context-pack via `Retriever`
- task submit/claim/status + worker loop via `Scheduler`
- topic rollup persistence metadata via `Codec` + `StateDelta`

Rollback is runtime-configurable via environment flags.
