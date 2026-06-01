# Claude Code Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `claude-code`
- Stable agent id: `claude_code_agent`

Session bootstrap:
```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=claude_code_agent
export MEMMCP_AGENT_ID=claude_code_agent
```

Preflight:
```bash
./scripts/agent_orchestration.sh preflight-agent claude-code contextlattice
# Or from globally installed wrapper (~/.contextlattice/bin):
contextlattice_agent_orchestration preflight-agent claude-code contextlattice
```

Behavior contract:
- Paste `docs/public_overview/templates/agents/universal.md` into system instructions.
