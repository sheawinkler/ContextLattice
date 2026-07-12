# Droid Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `droid`
- Stable agent id: `droid_agent`
- Optional install: `brew install --cask droid`

Use this for Droid or mobile/remote agents where local shell access may be mediated by another harness. Droid is an optional structured local/headless coding or review worker when the CLI is installed.

ContextLattice does not install Droid automatically. Runner execution is optional and adapter-first: ContextLattice packages context and records lifecycle; the Droid adapter owns local CLI invocation.

Paste into Droid/custom instructions when available:

```text
Use ContextLattice at http://127.0.0.1:8075 as the local memory and context contract.
Use stable agent_id droid_agent.
Before non-trivial work, run contextlattice context "<task>" --project <project> --pretty. Use MCP or HTTP only when the CLI is unavailable; never guess missing context.
Use explicit cwd or worktree for code mutation.
Use contextlattice_agent_adapter state to report idle, working, awaiting_user, blocked, or done when possible.
Write concise checkpoints for durable decisions and a short handoff before compaction, thread transfer, or account transfer.
Do not auto-merge, git push, or treat ContextLattice context as prompt filler.
Post-compaction readback is optional and bounded; use it only when the prior objective state is needed.
```

Operator workflow:

```bash
CONTEXTLATTICE_AGENT_ID=droid_agent contextlattice context "current task" --project contextlattice --pretty
contextlattice resume --project contextlattice --pretty
contextlattice remember "checkpoint summary" --project contextlattice --pretty
contextlattice finish "verified result" --success --project contextlattice --pretty
```

Optional runner execution:

```bash
TASK_AGENT=droid python3 scripts/task_agent_worker.py --task-agent droid --worker-name local-droid-01
```

The repo-local Droid adapter uses `droid exec --file <prompt> --cwd <path>` by default. Keep custom flags operator-controlled through `DROID_ARGS`, `DROID_AUTO_LEVEL`, `DROID_OUTPUT_FORMAT`, and `DROID_USE_SPEC`.
