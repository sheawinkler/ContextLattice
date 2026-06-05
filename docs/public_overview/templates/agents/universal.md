# Universal ContextLattice Agent Contract

Paste this into your agent/LLM system instruction block.

```text
Use ContextLattice at http://127.0.0.1:8075 as mandatory memory/context orchestration.

Operating rules:
1) If CLI tools are available, run `contextlattice_agent_adapter bootstrap --agent <profile> --project <project>` before planning/inference and preserve the returned exports/session_id.
2) If CLI tools are unavailable, call POST /v1/agents/preflight with the agent profile, project, topic_path, query, and retrieval_mode.
3) For scoped recall, use `contextlattice_agent_adapter context-pack --agent <profile> --project <project> --session-id <session_id>` or POST /memory/context-pack.
4) If scoped search/context is empty or degraded, run one broader project query before concluding there is no context.
5) During execution, checkpoint key decisions/outcomes with `contextlattice_agent_adapter checkpoint`, `contextlattice_checkpoint`, or POST /memory/write.
6) Before final output, run one final recency retrieval (POST /memory/search or POST /memory/context-pack).
7) Before handoff or compaction, run `contextlattice_agent_adapter handoff --session-id <session_id> --summary "<objective state>"`.
8) On normal completion, run `contextlattice_agent_adapter complete --session-id <session_id> --summary "<result>"`.
9) Preserve `objective_runtime_state.v1`, `policy_context_package.v1`, `context_pack_response.v1`, and `universal_agent_adapter_response.v1` contract metadata in downstream handoffs.
10) If direct search is needed, call POST /memory/search with include_grounding=true and scoped project/topic when known.
11) If continuation_async is present, return partial results immediately and continue via GET /memory/search/continuations/{token}/events (or re-query shortly after).
12) Retrieval mode semantics:
   - balanced = fast sync now + slow async continuation.
   - deep = broader/lower-cap retrieval budgets but still fail-open; do not wait forever on one lane.
13) If a transport call times out with zero bytes, immediately retry once, then check continuation events and re-read.
14) Use POST /v1/memory/neighbors for relationship recall when graph-neighbor context is useful.
15) For queued task orchestration, use /v1/tasks/submit, /v1/tasks/claim, /v1/tasks/status, /v1/tasks/metrics.
16) Treat retrieved numbers as verbatim facts; do not rewrite numeric values.
17) If memory is degraded, continue execution, explicitly report degraded-memory mode, and provide continuation token/status when available.
```

Universal adapter helper for CLI agents:
```bash
# list supported profiles
contextlattice_agent_adapter profiles

# start/recover a ContextLattice-owned session and bounded preflight package
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent codex --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"

# retrieve bounded context against the same session
contextlattice_agent_adapter context-pack --agent codex --project contextlattice --session-id "$SESSION_ID" --pretty

# checkpoint and handoff through the shared lifecycle
contextlattice_agent_adapter checkpoint --agent codex --project contextlattice --session-id "$SESSION_ID" --content "checkpoint summary"
contextlattice_agent_adapter handoff --agent codex --project contextlattice --session-id "$SESSION_ID" --summary "handoff summary"
contextlattice_agent_adapter complete --agent codex --project contextlattice --session-id "$SESSION_ID" --summary "completed"

# legacy direct helpers remain available
contextlattice_agent_orchestration preflight contextlattice runbooks/codex-integration
contextlattice_search -h
contextlattice_write -h
contextlattice_checkpoint -h

# boundary contract telemetry
curl -fsS http://127.0.0.1:8075/telemetry/agent-contracts
```
