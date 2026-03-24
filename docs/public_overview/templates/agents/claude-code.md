# Claude Code Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `claude-code`
- Stable agent id: `claude_code_agent`

Session bootstrap:
```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=claude_code_agent
export CONTEXTLATTICE_AGENT_ID=claude_code_agent
```

Preflight:
```bash
python3 scripts/agent_orchestration.py preflight-agent claude-code contextlattice
```

Rules:
1. Retrieve context before inference.
2. Use topic-scoped reads first; broaden only if scoped is empty/degraded.
3. Write concise checkpoints and final readback.
