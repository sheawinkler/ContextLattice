# Engine API (Phase 5)

This document defines the service boundary used when `CONTEXTLATTICE_ENGINE_MODE=service`.

## Protocol Contract

- Proto file: [`proto/contextlattice_engine.proto`](../proto/contextlattice_engine.proto)
- Service: `contextlattice.engine.v1.ContextEngineService`
- RPCs:
  - `PutMemory`
  - `GetMemory`
  - `QueryMemory`
  - `BatchPut`
  - `BatchQuery`
  - `GetStats`

## HTTP Compatibility Endpoints

The Python migration proxies currently use HTTP while gRPC is rolled out.

- `POST /v1/memory/put`
- `POST /v1/memory/update`
- `GET /v1/memory/get?memory_id=...`
- `POST /v1/memory/neighbors`
- `POST /v1/memory/batch-put`
- `POST /v1/retrieval/query`
- `POST /v1/retrieval/query-with-grounding`
- `POST /v1/retrieval/batch-query`
- `GET /v1/retrieval/health`

## Runtime Flags

- `USE_RUST_CODEC`
- `USE_RUST_MEMORY`
- `USE_RUST_RETRIEVAL`
- `USE_GO_ORCHESTRATOR`
- `CONTEXTLATTICE_ENGINE_MODE` (`embedded` or `service`)
- `CONTEXTLATTICE_ENGINE_URL` (Rust engine service URL)
- `CONTEXTLATTICE_GO_ORCHESTRATOR_URL` (Go scheduler URL)
- `MIGRATION_SHADOW_DUAL_RUN`
- `MIGRATION_CANARY_ENABLED`

## Embedded vs Service Mode

- `embedded`: Python callbacks execute in-process; this is the default.
- `service`: Rust/Go proxies are allowed to call remote engine services.

Compatibility mode:

- `CONTEXTLATTICE_ENGINE_URL=http://contextlattice-orchestrator:8075` enables service-mode cutover against built-in `/v1/memory/*` and `/v1/retrieval/*` compatibility endpoints while Rust engine binaries are rolled out incrementally.

## Rollback

Set all feature flags to `false` to route everything through the legacy Python path without deploy rollback.
