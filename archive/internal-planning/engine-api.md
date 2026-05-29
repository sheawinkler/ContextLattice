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
- `GET|POST /v1/memory/edges`
- `POST /v1/memory/edges/backfill`
- `POST /v1/memory/neighbors`
- `POST /v1/memory/batch-put`
- `POST /v1/retrieval/query`
- `POST /v1/retrieval/query-with-grounding`
- `POST /v1/retrieval/batch-query`
- `GET /v1/retrieval/health`

### Memory Edge Backfill

`POST /v1/memory/edges/backfill` is dry-run by default. It supports deterministic explicit backfill relations (`same_topic`, `references`, `same_session`, low-confidence `same_agent` audit rows) and opt-in bounded inferred scoring.

Key optional fields:

- `dry_run` (default `true`): set `false` to persist eligible edges.
- `relations`: restrict generated relations, e.g. `["inferred_related"]`.
- `min_confidence` (default `0.95`): write threshold for all generated edges.
- `include_inferred` (default `false`): enables bounded same-project `inferred_related` scoring.
- `inferred_peer_limit` (default `2`): maximum inferred peers considered per source memory.
- `inferred_scan_limit` (default `5000`): maximum docs scanned for inferred scoring.
- `inferred_min_score` (default `0.90`): minimum inferred score to report.
- `inferred_min_shared_terms` (default `3`): minimum lexical overlap before scoring.
- `inferred_max_token_postings` (default `64`): skips overly-common terms to bound fanout.
- `corpus` (default `history_index`): `history_index` uses the live bounded hot index; `disk` scans project files and is intended for older project-scoped retrofills.

Operator-safe retrofill wrapper:

```bash
./scripts/agent/memory-edge-inferred-retrofill --project context-lattice-private
./scripts/agent/memory-edge-inferred-retrofill --project context-lattice-private --profile exploratory
./scripts/agent/memory-edge-inferred-retrofill --project context-lattice-private --profile exploratory --write --confirm-retrofill context-lattice-private
./scripts/agent/memory-edge-inferred-retrofill --project hermes-agent-ultra --corpus disk --profile exploratory
```

The wrapper restricts the request to `inferred_related`, runs a dry-run preflight before any write, refuses truncated preflight results unless `--allow-truncated` is set, and repeats write mode once to verify idempotency. Profiles provide density presets: `strict` (`0.90`, peer `1`, postings `64`), `balanced` (`0.85`, peer `3`, postings `128`), and `exploratory` (`0.80`, peer `5`, postings `256`). Explicit flags override the selected profile.

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
