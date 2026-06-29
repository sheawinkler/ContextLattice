# Droid Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `droid`
- Stable agent id: `droid_agent`

Use this for Droid or mobile/remote agents where local shell access may be mediated by another harness.

Paste into Droid/custom instructions when available:

```text
Use ContextLattice at http://127.0.0.1:8075 as the local memory and context contract.
Use stable agent_id droid_agent.
Before non-trivial work, obtain a scoped ContextLattice bootstrap/context pack through CLI, MCP, or HTTP. If no tool path is available, ask for the current context pack instead of guessing.
Write concise checkpoints for durable decisions and a short handoff before compaction, thread transfer, or account transfer.
Post-compaction readback is optional and bounded; use it only when the prior objective state is needed.
```

Operator preflight:

```bash
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent droid --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
contextlattice_agent_session context-package --session-id "$SESSION_ID" --pretty
contextlattice_agent_trace --session-id "$SESSION_ID" --tree
```
