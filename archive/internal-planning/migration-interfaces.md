# Migration Interface Proposals (Phase 0 Output)

This document proposes the stable interfaces required before Rust/Go substitution.

## 1) Codec

Purpose: serialization/deserialization boundary.

Required operations:

- `encode_state(obj) -> bytes`
- `decode_state(bytes) -> obj`
- `encode_batch(items) -> bytes`
- `decode_batch(bytes) -> list[obj]`
- `checksum(bytes) -> str`

## 2) MemoryStore

Purpose: context memory graph boundary.

Required operations:

- `add_memory(input) -> memory_id`
- `update_memory(memory_id, patch) -> bool`
- `get_memory(memory_id) -> memory`
- `upsert_memory_edge(source_id, target_id, relation, metadata) -> edge_id`
- `query_neighbors(memory_id, filters) -> list[memory]`
- `batch_insert(items) -> list[memory_id]`

## 3) Retriever

Purpose: retrieval and ranking boundary.

Required operations:

- `search(query, filters, limit) -> list[result]`
- `batch_search(queries, filters, limit) -> list[list[result]]`
- `search_with_grounding(query, filters, limit) -> grounded_result`
- `health() -> retriever_health`

## 4) Scheduler

Purpose: orchestration/scheduling boundary.

Required operations:

- `submit_task(task) -> task_id`
- `claim_next(worker_id) -> task | None`
- `update_status(task_id, status, metadata) -> bool`
- `retry(task_id) -> bool`
- `queue_metrics() -> queue_metrics`

## 5) StateDelta

Purpose: patch-oriented state updates.

Required operations:

- `diff(old_state, new_state) -> delta`
- `apply(state, delta) -> state`
- `compose(delta_a, delta_b) -> delta`
- `validate(delta) -> bool`

## Feature Flag Contract

Every future substitution must be gated and rollback-safe.

- `USE_RUST_CODEC`
- `USE_RUST_MEMORY`
- `USE_RUST_RETRIEVAL`
- `USE_GO_ORCHESTRATOR`

Rules:

1. Default path remains current Python implementation until parity is proven.
2. Rust/Go implementations run in shadow mode before default cutover.
3. Rollback to Python must be runtime-configurable with no deploy rollback required.
