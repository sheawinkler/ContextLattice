# Migration Phase Status

Date: 2026-03-04

## Current Language Ownership (v2+ runtime)

| Layer | Primary language | Responsibilities | Why this improves efficacy |
|---|---|---|---|
| Control plane | Python | API ingress, auth/policy, telemetry, integration surface, fallback routing | Maximizes iteration speed and keeps rollback/fallback behavior explicit and stable |
| Memory + retrieval hot path | Rust | Codec, memory engine, retrieval engine | Reduces p95/p99 latency tails and improves deep-read stability under load |
| Scheduling + coordination | Go | Task scheduling, batching, retries, backpressure, service coordination | Improves queue/task reliability and reduces orchestration stalls |

Runtime verification endpoint: `/migration/runtime`

## Phase 1: Stable Interface Extraction

Implemented in code:

- Runtime interfaces: `Codec`, `MemoryStore`, `Retriever`, `Scheduler`, `StateDelta`
- Runtime flags: `USE_RUST_CODEC`, `USE_RUST_MEMORY`, `USE_RUST_RETRIEVAL`, `USE_GO_ORCHESTRATOR`
- Hot-path routing:
  - `/memory/search` and `/memory/context-pack` use retriever adapter
  - Task submit/claim/status + worker loop use scheduler adapter
  - Topic rollup persistence uses codec + state-delta metadata

## Phase 2: Rust Codec

Scaffolded:

- Rust crate: [`crates/context_codec`](/Users/sheawinkler/.mcp-servers/mem_mcp_lobehub/crates/context_codec)
- Python runtime bridge with fallback: `RustCodecBridge`

## Phase 3: Rust Memory Engine

Scaffolded:

- Rust crate: [`crates/context_engine`](/Users/sheawinkler/.mcp-servers/mem_mcp_lobehub/crates/context_engine)
- Python runtime proxy with fallback: `RustMemoryStoreProxy`

## Phase 4: Rust Retrieval Engine

Scaffolded:

- Rust crate: [`crates/context_retrieval`](/Users/sheawinkler/.mcp-servers/mem_mcp_lobehub/crates/context_retrieval)
- Python runtime proxy with fallback: `RustRetrieverProxy`

## Phase 5: Engine Service Layer

Added:

- Proto contract: [`proto/contextlattice_engine.proto`](/Users/sheawinkler/.mcp-servers/mem_mcp_lobehub/proto/contextlattice_engine.proto)
- Service API doc: [`docs/engine-api.md`](/Users/sheawinkler/.mcp-servers/mem_mcp_lobehub/docs/engine-api.md)
- Runtime endpoint: `/migration/runtime`

## Phase 6: Go Orchestration Layer

Scaffolded:

- Go scheduler service: [`services/orchestrator-go`](/Users/sheawinkler/.mcp-servers/mem_mcp_lobehub/services/orchestrator-go)
- Go gateway service: [`services/gateway-go`](/Users/sheawinkler/.mcp-servers/mem_mcp_lobehub/services/gateway-go)
- Python proxy with fallback: `GoSchedulerProxy`

## Phase 7: Latency Reduction

Implemented incrementally in Python path:

- Existing staged fetch and pathway caching preserved
- Adapter routing keeps retrieval fast-path in-process when service mode is off
- Topic rollup persistence now stores state-delta stats for smaller change tracking

## Phase 8: Hardening and Cutover

Added controls:

- `MIGRATION_SHADOW_DUAL_RUN`
- `MIGRATION_CANARY_ENABLED`
- Runtime introspection endpoint for canary/rollback verification

Remaining to fully complete production cutover:

- dual-run parity assertions against live traffic
- shadow/canary automation and rollback playbooks in CI/CD
- benchmark validation against Phase 0 baseline
