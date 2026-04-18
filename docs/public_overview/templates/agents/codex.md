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
./scripts/agent_orchestration.sh preflight contextlattice runbooks/codex-integration
# Or from any working directory:
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
python3 "$REPO_ROOT/scripts/agent_orchestration.py" preflight contextlattice runbooks/codex-integration
```

Behavior contract:
- Paste `docs/public_overview/templates/agents/universal.md` into system instructions.
