# Universal ContextLattice Agent Contract

Paste this into your agent/LLM system instruction block.

```text
Use ContextLattice at http://127.0.0.1:8075 as mandatory memory/context orchestration.

Readiness rule: when starting on a new machine, account, or agent surface, run `contextlattice_adopt status --pretty` first. If local repo instructions are missing, run `contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,pi,droid --pretty`.

Operating rules:
1) If CLI tools are available, run `contextlattice_agent_adapter bootstrap --agent <profile> --project <project>` before planning/inference and preserve the returned exports/session_id.
2) If CLI tools are unavailable, call POST /v1/agents/preflight with the agent profile, project, topic_path, query, and retrieval_mode.
3) For scoped recall, use `contextlattice_agent_adapter context-pack --agent <profile> --project <project> --session-id <session_id>` or POST /memory/context-pack.
4) Report semantic agent state when it changes: `contextlattice_agent_adapter state --agent <profile> --session-id <session_id> --state working|awaiting_user|blocked|done --summary "<why>"`.
5) If scoped search/context is empty or degraded, run one broader project query before concluding there is no context.
6) During execution, checkpoint key decisions/outcomes with `contextlattice_agent_adapter checkpoint`, `contextlattice_checkpoint`, or POST /memory/write.
7) Before final output, run a final recency retrieval only when the work was long-running, high-risk, or likely affected by recent memory.
8) Before handoff or compaction, run `contextlattice_agent_adapter handoff --session-id <session_id> --summary "<objective state>"`.
9) When preparing a difficult new model/problem-solving request, run `contextlattice_agent_session context-package --session-id <session_id>` and use the returned reference package as the factual context scaffold, including the project/topic/session objective lineage.
10) When you need to explain what shaped the run, use `contextlattice_agent_trace --session-id <session_id> --tree` or GET /v1/agents/sessions/{session_id}/trace; the trace includes objective lineage, context, skills, sources, graph touches, handoffs, checkpoints, lifecycle state, and ownership.
11) On normal completion, run `contextlattice_agent_adapter complete --session-id <session_id> --summary "<result>"`.
12) Preserve `objective_runtime_state.v1`, `policy_context_package.v1`, `context_pack_response.v1`, `agent_session_rollup.v1`, `agent_prompt_context_package.v1`, `agent_run_trace.v1`, `contextlattice_agent_lifecycle_state.v1`, and `universal_agent_adapter_response.v1` contract metadata, including `objective_hierarchy` and `objective_lineage`, in downstream handoffs.
13) If direct search is needed, call POST /memory/search with include_grounding=true and scoped project/topic when known.
14) If relevant capabilities are unclear, run `contextlattice_skills_index search "<task or tool need>" --pretty` instead of loading every skill.
15) If continuation_async is present, return partial results immediately and continue via GET /memory/search/continuations/{token}/events, or run the returned agent_visibility.watch_command so the requesting session receives the completion/degraded steering comment.
16) Retrieval mode semantics:
   - balanced = fast sync now + slow async continuation.
   - deep = broader/lower-cap retrieval budgets but still fail-open; do not wait forever on one lane.
17) Agent lifecycle and retrieval lifecycle are separate: agent state is idle/working/awaiting_user/blocked/done, while retrieval lifecycle is source-fetch progress.
18) If a transport call times out with zero bytes, immediately retry once, then check continuation events. Use readback only when recovering prior objective state.
19) Use POST /v1/memory/neighbors for relationship recall when graph-neighbor context is useful.
20) For queued task orchestration, use /v1/tasks/submit, /v1/tasks/claim, /v1/tasks/status, /v1/tasks/metrics.
21) Treat retrieved numbers as verbatim facts; do not rewrite numeric values.
22) If memory is degraded, continue execution, explicitly report degraded-memory mode, and provide continuation token/status when available.
```

Universal adapter helper for CLI agents:
```bash
# list supported profiles
contextlattice_adopt status --pretty
contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,pi,droid --pretty
contextlattice_agent_adapter profiles

# start/recover a ContextLattice-owned session and bounded preflight package
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent codex --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"

# retrieve bounded context against the same session
contextlattice_agent_adapter context-pack --agent codex --project contextlattice --session-id "$SESSION_ID" --pretty
contextlattice_agent_adapter state --agent codex --project contextlattice --session-id "$SESSION_ID" --state working --pretty

# checkpoint and handoff through the shared lifecycle
contextlattice_agent_adapter checkpoint --agent codex --project contextlattice --session-id "$SESSION_ID" --content "checkpoint summary"
contextlattice_agent_adapter handoff --agent codex --project contextlattice --session-id "$SESSION_ID" --summary "handoff summary"
contextlattice_agent_adapter complete --agent codex --project contextlattice --session-id "$SESSION_ID" --summary "completed"

# package session state for the next model call
contextlattice_agent_session rollup --session-id "$SESSION_ID" --pretty
contextlattice_agent_session context-package --session-id "$SESSION_ID" --pretty
contextlattice_agent_trace --session-id "$SESSION_ID" --tree
contextlattice_agent_discover --agents codex,claude-code,opencode --repo . --pretty

# discover capabilities without bloating the prompt
contextlattice_skills_index search "browser automation" --pretty

# direct helpers remain available
contextlattice_search -h
contextlattice_write -h
contextlattice_checkpoint -h

# boundary contract telemetry
curl -fsS http://127.0.0.1:8075/telemetry/agent-contracts
```
