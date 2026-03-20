# OpenCode Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `opencode`
- Stable agent id: `opencode_agent`

Session bootstrap:
```bash
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=opencode_agent
export MEMMCP_AGENT_ID=opencode_agent
```

Preflight:
```bash
python3 scripts/agent_orchestration.py preflight-agent opencode contextlattice
```

Rules:
1. Pre-inference `POST /memory/search` with `include_grounding=true`.
2. Use `POST /memory/context-pack` when task touches multiple files.
3. Write progress decisions with `POST /memory/write`.
