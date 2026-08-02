# V4 Runtime And Toolset Decision

Decision date: 2026-08-01.

This record closes the broad V4 deliberation by naming the supported role of
each component. New experiments require a focused baseline/holdout ledger; they
do not reopen the entire runtime stack.

## Retrieval and memory lanes

### Letta

Letta is an additive full-stack connector for archival and slow/deep recall. It
is not native memory truth and is not allowed to block the default fast path.
Per-mode top-k caps, bounded concurrency, backlog admission, timeouts, cooldowns,
and durable async continuation contain its cost and tail latency. Operators may
disable the lane without invalidating native memory or Qdrant-backed retrieval.

### MindsDB

MindsDB remains an optional analytics/connector surface in the full/operator
stack. Lite explicitly disables it, native memory and Qdrant do not depend on
it, and `ORCH_MINDSDB_FANOUT_ENABLED` defaults false. A future promotion requires
a unique capability and measured quality or operational gain that the native
lanes cannot provide.

## Async retrieval UX

Slow work is represented as durable continuation state rather than a silent
timeout. Responses expose lifecycle, pending/warming/failed sources, modeled
progress, poll URLs, and SSE event URLs. Modeled progress is advisory and must
never be presented as deterministic completion time. Terminal source failures
remain explicit; pending or warming work is partial, not falsely failed.

## Local inference

- Go owns the inference routing contract.
- MLX is a first-class opt-in provider for Apple Silicon and may be selected by
  hardware-aware policy when configured and healthy.
- Ollama remains the compatibility fallback and the default local service in the
  full Compose stack; it is not the preferred embedding hot path.
- `TASK_MODEL=qwen3.5:9b` is the bounded task default. Dream Mode replaces
  deprecated `qwen2.5-coder` requests with a supported Qwen 3.x fallback.
- FastEmbed with `BAAI/bge-small-en-v1.5` is the default embedding lane.
  `nomic-embed-text` remains an optional Letta seed helper, not the system-wide
  embedding default.
- Resource-sensitive backend experiments run one at a time. The single-active
  inference guard remains enabled by default.

The opt-in model shortlist and conformance commands remain in
[Local Model Options](local-model-options.md). A model name in that list is a
candidate, not a bundled asset or performance claim.

## Reopening gates

Open a focused issue only when evidence identifies a concrete gap. It must
include a baseline, representative holdout, quality and latency metrics, memory
and cost impact, failure evidence, a reproduction artifact, and rollback. Model
recency or repository popularity alone is not a promotion reason.
