# Hermes Agent Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `hermes-agent`
- Stable agent id: `hermes_agent`

Session bootstrap:
```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=hermes_agent
export CONTEXTLATTICE_AGENT_ID=hermes_agent
```

Preflight:
```bash
python3 scripts/agent_orchestration.py preflight-agent hermes-agent contextlattice
```

Rules:
1. Run scoped retrieval first.
2. Submit feedback via `POST /tools/feedback_submit` for learning.
3. Keep numbers as verbatim copies from retrieved data.
