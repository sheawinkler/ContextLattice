# Claude Web/Desktop Integration Template

Paste into a new Claude session instruction:

```text
Use ContextLattice at http://127.0.0.1:8075.
Use stable agent_id claude_web_agent (or claude_desktop_agent).
Follow the full operating contract in docs/public_overview/templates/agents/universal.md.
```

Operator preflight:
```bash
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent claude-web --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
contextlattice_agent_session context-package --session-id "$SESSION_ID" --pretty
contextlattice_agent_trace --session-id "$SESSION_ID" --tree
contextlattice_skills_index search "current task capability" --pretty

# Direct HTTP fallback for web-only environments:
curl -fsS -H "content-type: application/json" -H "x-api-key: ${CONTEXTLATTICE_ORCHESTRATOR_API_KEY}" \
  -d '{"agent":"claude-web","project":"contextlattice"}' \
  http://127.0.0.1:8075/v1/agents/preflight | jq
```

For a hard follow-up prompt, provide Claude the returned `reference_prompt` from the session context package instead of raw logs. Use the run trace when you need a compact explanation of which context, skills, graph touches, and handoffs shaped the work.
