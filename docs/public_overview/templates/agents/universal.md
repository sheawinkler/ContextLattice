# Universal ContextLattice Agent Contract

Paste this into your agent/LLM system instruction block.

```text
Use ContextLattice at http://127.0.0.1:8075 as mandatory memory/context orchestration.

Operating rules:
1) Before any planning/inference, call POST /memory/search with include_grounding=true and scoped project/topic when known.
2) If scoped search is empty or degraded, run one broader search in the same project before concluding there is no context.
3) For broad multi-file work, call POST /memory/context-pack.
4) During execution, checkpoint key decisions/outcomes with POST /memory/write or contextlattice_checkpoint.
5) Prefer hook-first startup when CLI tools are installed: contextlattice_agent_start --soft --compact.
6) Before final output, run one final recency retrieval (POST /memory/search or POST /memory/context-pack).
7) If continuation_async is present, return partial results immediately and continue via GET /memory/search/continuations/{token}/events (or re-query shortly after).
8) Retrieval mode semantics:
   - balanced = fast sync now + slow async continuation.
   - deep = broader/lower-cap retrieval budgets but still fail-open; do not wait forever on one lane.
9) If a transport call times out with zero bytes, immediately retry once, then check continuation events and re-read.
10) Use POST /v1/memory/neighbors for relationship recall when graph-neighbor context is useful.
11) Use profile-aware preflight via POST /v1/agents/preflight before major tasks.
12) For queued task orchestration, use /v1/tasks/submit, /v1/tasks/claim, /v1/tasks/status, /v1/tasks/metrics.
13) Treat retrieved numbers as verbatim facts; do not rewrite numeric values.
14) Before context compaction, run compaction handoff write+readback so objective state survives:
    contextlattice_agent_orchestration compaction-handoff contextlattice "<objective summary>" runbooks/context-compaction-handoff balanced

If memory is degraded, continue execution, explicitly report degraded-memory mode, and provide continuation token/status when available.
```

Preflight command helper for CLI agents:
```bash
# repo-root invocation
python3 scripts/agent_orchestration.py preflight contextlattice runbooks/codex-integration

# any-working-directory invocation
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
python3 "$REPO_ROOT/scripts/agent_orchestration.py" preflight contextlattice runbooks/codex-integration

# global wrapper invocation (auto-installed by quickstart/installers)
contextlattice_agent_orchestration preflight contextlattice runbooks/codex-integration

# global retrieval/write helpers
contextlattice_search -h
contextlattice_write -h
contextlattice_agent_start -h
contextlattice_checkpoint -h

# compaction handoff helper (default before summary compaction)
contextlattice_agent_orchestration compaction-handoff contextlattice "objective summary" runbooks/context-compaction-handoff balanced
```
