# Human Agent Instruction Playbook

Use this playbook to connect common agent clients to ContextLattice with consistent behavior.

## Canonical Runtime

- Orchestrator endpoint: `http://127.0.0.1:8075`
- API key: `CONTEXTLATTICE_ORCHESTRATOR_API_KEY`
- Required behaviors for all agents:
  1. Retrieve context before inference (`POST /memory/search`, `include_grounding=true`).
  2. Use `POST /memory/context-pack` for broad or multi-file tasks.
  3. Checkpoint long-task progress (`POST /memory/write`).
  4. Run one final recency retrieval before final output.
  5. If memory is degraded, continue execution and report degraded-memory mode.

## Profile-Aware Preflight

Prefer profile-aware preflight so each agent gets a stable `agent_id`, topic scope, and preflight query.

```bash
ORCH_KEY="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env)"

curl -fsS -H "content-type: application/json" -H "x-api-key: ${ORCH_KEY}" \
  -d '{"agent":"claude-code","project":"contextlattice"}' \
  http://127.0.0.1:8075/v1/agents/preflight | jq
```

Supported `agent` values:

- `codex`
- `claude-code`
- `opencode`
- `hermes-agent`
- `chatgpt-web`
- `chatgpt-desktop`
- `claude-web`
- `claude-desktop`

Compatibility alias:

- `POST /v1/codex/preflight`

## Local Helper Script

```bash
python3 scripts/agent_orchestration.py preflight contextlattice runbooks/codex-integration
python3 scripts/agent_orchestration.py preflight-agent claude-code contextlattice
python3 scripts/agent_orchestration.py preflight-agent opencode contextlattice
python3 scripts/agent_orchestration.py preflight-agent hermes-agent contextlattice
```

## Template Packs

Use copy-ready templates in:

- `docs/public_overview/templates/agents/codex.md`
- `docs/public_overview/templates/agents/claude-code.md`
- `docs/public_overview/templates/agents/opencode.md`
- `docs/public_overview/templates/agents/hermes-agent.md`
- `docs/public_overview/templates/agents/chatgpt-web-desktop.md`
- `docs/public_overview/templates/agents/claude-web-desktop.md`
