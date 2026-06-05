# Codex Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `codex`
- Stable agent id: `codex_gpt5`

Session bootstrap:
```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=codex_gpt5
export MEMMCP_AGENT_ID=codex_gpt5
```

Preflight:
```bash
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent codex --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
contextlattice_agent_adapter context-pack --agent codex --project contextlattice --session-id "$SESSION_ID" --pretty

# Compatibility wrapper:
contextlattice_agent_orchestration preflight contextlattice runbooks/codex-integration
```

Behavior contract:
- Paste `docs/public_overview/templates/agents/universal.md` into system instructions.
- Codex SessionStart hook template: `config/codex/contextlattice_agent_start.sh`
- Install Codex hooks: `scripts/install_global_agent_tools.sh --install-codex-hooks`
