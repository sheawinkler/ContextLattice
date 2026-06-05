# Hermes Agent Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `hermes-agent`
- Stable agent id: `hermes_agent`

Session bootstrap:
```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=hermes_agent
export MEMMCP_AGENT_ID=hermes_agent
```

Preflight:
```bash
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent hermes-agent --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
contextlattice_agent_adapter context-pack --agent hermes-agent --project contextlattice --session-id "$SESSION_ID" --pretty

# Compatibility wrapper:
contextlattice_agent_orchestration preflight-agent hermes-agent contextlattice
```

Behavior contract:
- Paste `docs/public_overview/templates/agents/universal.md` into system instructions.
