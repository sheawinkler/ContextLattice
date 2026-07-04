# Pi Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `pi`
- Stable agent id: `pi_agent`
- Optional install: `brew install pi-coding-agent`

For Pi-style conversational agents, keep the instruction lightweight and use ContextLattice as a memory briefing layer, not as a wall of pasted logs. Pi is best treated as an optional scout, summarizer, reviewer, or lightweight refactor runner when the local CLI exists.

ContextLattice does not install Pi automatically. Runner execution is optional and adapter-first: ContextLattice packages context and records lifecycle; the Pi adapter owns local CLI invocation.

Paste into Pi/custom instructions when available:

```text
Use ContextLattice at http://127.0.0.1:8075 for durable memory, scoped recall, and handoff continuity.
Use stable agent_id pi_agent.
Before non-trivial work, ask the operator or local harness for a ContextLattice bootstrap/context pack. If unavailable, continue from local evidence and say degraded-memory mode.
Use contextlattice_agent_adapter state to report idle, working, awaiting_user, blocked, or done when possible.
Checkpoint durable decisions and write a concise handoff before compaction or account/thread transfer.
Do not auto-merge, git push, or treat ContextLattice context as prompt filler.
Post-compaction readback is optional and bounded; use it only to recover prior objective state, not as prompt filler.
```

Operator preflight:

```bash
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent pi --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
contextlattice_agent_session context-package --session-id "$SESSION_ID" --pretty
contextlattice_agent_trace --session-id "$SESSION_ID" --tree
```

Optional runner execution:

```bash
TASK_AGENT=pi python3 scripts/task_agent_worker.py --task-agent pi --worker-name local-pi-01
```
