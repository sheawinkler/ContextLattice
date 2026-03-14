# fastembed-rs

- Track: `#69` performance shortlist
- Status: `spike implemented (feature-flagged)`
- Why: high potential for embedding throughput gains in Rust path
- Gate:
  - p95 latency improvement >= 20% on embedding-heavy workloads
  - no recall-quality regression
  - timeout/error non-regression

## Implemented in app runtime

- `embed_text_batch(...)` added to orchestrator and used by Qdrant batch fanout writes.
- fastembed adapter now supports batched calls with telemetry counters:
  - `batchCalls`
  - `batchItems`
  - `batchFailures`
- shortlist harness now includes `embedding_stress` case and captures adapter telemetry snapshots (`before/after`).
