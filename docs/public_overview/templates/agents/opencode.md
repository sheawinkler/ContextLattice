# OpenCode Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `opencode`
- Stable agent id: `opencode_agent`

Session bootstrap:
```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=opencode_agent
export MEMMCP_AGENT_ID=opencode_agent
```

Preflight:
```bash
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent opencode --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
contextlattice_agent_adapter context-pack --agent opencode --project contextlattice --session-id "$SESSION_ID" --pretty
contextlattice_agent_session context-package --session-id "$SESSION_ID" --pretty
contextlattice_skills_index search "repo conventions testing release" --pretty

# Compatibility wrapper:
contextlattice_agent_orchestration preflight-agent opencode contextlattice
```

Behavior contract:
- Paste `docs/public_overview/templates/agents/universal.md` into system instructions.
- Before a difficult model call, use the session context package as the bounded factual scaffold.
