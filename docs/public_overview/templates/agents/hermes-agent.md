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
python3 scripts/agent_orchestration.py preflight-agent hermes-agent contextlattice
```

Behavior contract:
- Paste `docs/public_overview/templates/agents/universal.md` into system instructions.
