# Claude Web/Desktop Integration Template

Paste into a new Claude session instruction:

```text
Use ContextLattice at http://127.0.0.1:8075.
Use stable agent_id claude_web_agent (or claude_desktop_agent).
Follow the full operating contract in docs/public_overview/templates/agents/universal.md.
```

Operator preflight:
```bash
CONTEXTLATTICE_AGENT_ID=claude_web_agent contextlattice context "current task" --project contextlattice --pretty
contextlattice resume --project contextlattice --pretty
contextlattice_skills_index search "current task capability" --pretty

# Direct HTTP fallback for web-only environments:
curl -fsS -H "content-type: application/json" -H "x-api-key: ${CONTEXTLATTICE_ORCHESTRATOR_API_KEY}" \
  -d '{"agent":"claude-web","project":"contextlattice"}' \
  http://127.0.0.1:8075/v1/agents/preflight | jq
```

For a hard follow-up prompt, provide Claude the compact packet instead of raw logs. Use the run trace only for advanced provenance debugging.
