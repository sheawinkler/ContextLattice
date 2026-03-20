# ChatGPT Web/Desktop Integration Template

Paste into a new ChatGPT session:

```text
Use ContextLattice as memory/context at http://127.0.0.1:8075.
Before planning, call POST /memory/search with include_grounding=true.
For broad tasks, call POST /memory/context-pack.
During long tasks, checkpoint via POST /memory/write.
Before final answer, run one recency read.
If memory is degraded, continue and state degraded-memory mode.
Use agent profile chatgpt-web (or chatgpt-desktop) and stable agent_id chatgpt_web_agent (or chatgpt_desktop_agent).
```

Operator preflight:
```bash
curl -fsS -H "content-type: application/json" -H "x-api-key: ${CONTEXTLATTICE_ORCHESTRATOR_API_KEY}" \
  -d '{"agent":"chatgpt-web","project":"contextlattice"}' \
  http://127.0.0.1:8075/v1/agents/preflight | jq
```
