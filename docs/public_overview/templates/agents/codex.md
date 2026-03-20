# Codex Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `codex`
- Stable agent id: `codex_gpt5`

Session bootstrap:
```bash
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=codex_gpt5
export MEMMCP_AGENT_ID=codex_gpt5
```

Preflight:
```bash
python3 scripts/agent_orchestration.py preflight contextlattice runbooks/codex-integration
```

Rules:
1. Always run `POST /memory/search` before planning/coding.
2. Use `POST /memory/context-pack` for broad tasks.
3. Checkpoint via `POST /memory/write` during long tasks.
4. Run one final recency read before final output.
