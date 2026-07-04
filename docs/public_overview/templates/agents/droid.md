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
Before non-trivial work, obtain a scoped ContextLattice bootstrap/context pack through CLI, MCP, or HTTP. If no tool path is available, ask for the current context pack instead of guessing.
Use explicit cwd or worktree for code mutation.
Use contextlattice_agent_adapter state to report idle, working, awaiting_user, blocked, or done when possible.
Write concise checkpoints for durable decisions and a short handoff before compaction, thread transfer, or account transfer.
Do not auto-merge, git push, or treat ContextLattice context as prompt filler.
Post-compaction readback is optional and bounded; use it only when the prior objective state is needed.
```

Operator preflight:

```bash
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent droid --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
contextlattice_agent_session context-package --session-id "$SESSION_ID" --pretty
contextlattice_agent_trace --session-id "$SESSION_ID" --tree
```

Optional runner execution:

```bash
TASK_AGENT=droid python3 scripts/task_agent_worker.py --task-agent droid --worker-name local-droid-01
```
