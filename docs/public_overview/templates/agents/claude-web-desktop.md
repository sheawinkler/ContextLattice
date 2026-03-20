# Claude Web/Desktop Integration Template

Paste into a new Claude session:

```text
Use ContextLattice as memory/context at http://127.0.0.1:8075.
Before planning, call POST /memory/search with include_grounding=true.
Use topic_path whenever known; broaden if scoped results are empty.
For broad tasks, call POST /memory/context-pack.
Checkpoint via POST /memory/write and do one final recency retrieval.
If memory is degraded, continue and report degraded-memory mode.
Use agent profile claude-web (or claude-desktop) with stable agent_id claude_web_agent (or claude_desktop_agent).
```

Operator preflight:
```bash
curl -fsS -H "content-type: application/json" -H "x-api-key: ${CONTEXTLATTICE_ORCHESTRATOR_API_KEY}" \
  -d '{"agent":"claude-web","project":"contextlattice"}' \
  http://127.0.0.1:8075/v1/agents/preflight | jq
```
