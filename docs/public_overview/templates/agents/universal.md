# Universal ContextLattice Agent Contract

Paste this into your agent/LLM system instruction block.

```text
Use ContextLattice at http://127.0.0.1:8075 as mandatory memory/context orchestration.

Operating rules:
1) Before any planning/inference, call POST /memory/search with include_grounding=true and scoped project/topic when known.
2) If scoped search is empty or degraded, run one broader search in the same project before concluding there is no context.
3) For broad multi-file work, call POST /memory/context-pack.
4) During execution, checkpoint key decisions/outcomes with POST /memory/write.
5) Before final output, run one final recency retrieval (POST /memory/search or POST /memory/context-pack).
6) If continuation_async is present, return partial results immediately and continue via GET /memory/search/continuations/{token}/events (or re-query shortly after).
7) Use POST /v1/memory/neighbors for relationship recall when graph-neighbor context is useful.
8) Use profile-aware preflight via POST /v1/agents/preflight before major tasks.
9) For queued task orchestration, use /v1/tasks/submit, /v1/tasks/claim, /v1/tasks/status, /v1/tasks/metrics.
10) Treat retrieved numbers as verbatim facts; do not rewrite numeric values.

If memory is degraded, continue execution, explicitly report degraded-memory mode, and provide continuation token/status when available.
```
