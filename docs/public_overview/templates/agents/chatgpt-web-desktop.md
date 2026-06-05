# ChatGPT Web/Desktop Integration Template

Paste into a new ChatGPT session system/custom instruction:

```text
Use ContextLattice at http://127.0.0.1:8075.
Use stable agent_id chatgpt_web_agent (or chatgpt_desktop_agent).
Follow the full operating contract in docs/public_overview/templates/agents/universal.md.
```

Operator preflight:
```bash
contextlattice_agent_adapter bootstrap --agent chatgpt-web --project contextlattice --pretty

# Direct HTTP fallback for web-only environments:
curl -fsS -H "content-type: application/json" -H "x-api-key: ${CONTEXTLATTICE_ORCHESTRATOR_API_KEY}" \
  -d '{"agent":"chatgpt-web","project":"contextlattice"}' \
  http://127.0.0.1:8075/v1/agents/preflight | jq
```
