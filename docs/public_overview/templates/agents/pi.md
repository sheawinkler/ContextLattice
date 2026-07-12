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
Before non-trivial work, run contextlattice context "<task>" --project <project> --pretty. If the CLI is unavailable, continue from local evidence and say degraded-memory mode.
Use contextlattice_agent_adapter state to report idle, working, awaiting_user, blocked, or done when possible.
Checkpoint durable decisions and write a concise handoff before compaction or account/thread transfer.
Do not auto-merge, git push, or treat ContextLattice context as prompt filler.
Post-compaction readback is optional and bounded; use it only to recover prior objective state, not as prompt filler.
```

Operator workflow:

```bash
CONTEXTLATTICE_AGENT_ID=pi_agent contextlattice context "current task" --project contextlattice --pretty
contextlattice resume --project contextlattice --pretty
contextlattice remember "checkpoint summary" --project contextlattice --pretty
contextlattice finish "verified result" --success --project contextlattice --pretty
```

Optional runner execution:

```bash
TASK_AGENT=pi python3 scripts/task_agent_worker.py --task-agent pi --worker-name local-pi-01
```
