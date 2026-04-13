# Human Agent Instruction Playbook

Use this playbook to connect agent clients to ContextLattice with consistent, low-friction behavior.

## 1) One-minute operator bootstrap

```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=codex_gpt5
export MEMMCP_AGENT_ID=codex_gpt5
export ORCH_KEY="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env | tail -1)"
```

## 2) Canonical instruction block for any agent

Use this source of truth:
- `docs/public_overview/templates/agents/universal.md`

## 3) Preflight (profile-aware)

```bash
curl -fsS -H "content-type: application/json" -H "x-api-key: ${ORCH_KEY}" \
  -d '{"agent":"codex","project":"contextlattice"}' \
  http://127.0.0.1:8075/v1/agents/preflight | jq
```

Supported profiles:
- `codex`
- `claude-code`
- `opencode`
- `hermes-agent`
- `chatgpt-web`, `chatgpt-desktop`
- `claude-web`, `claude-desktop`

## 4) End-to-end operational loop

1. Read before inference: `POST /memory/search` (`include_grounding=true`, scoped `project/topic_path`).
2. Broaden once if scoped read is empty/degraded.
3. For broad tasks: `POST /memory/context-pack`.
4. During execution: `POST /memory/write` checkpoints.
5. Before final output: one recency retrieval (`/memory/search` or `/memory/context-pack`).
6. For graph relationships: `POST /v1/memory/neighbors`.
7. For async continuation: use `continuation_async` token and stream `GET /memory/search/continuations/{token}/events`.
8. For queued orchestration: `/v1/tasks/submit`, `/v1/tasks/claim`, `/v1/tasks/status`, `/v1/tasks/metrics`.

## 5) Minimal smoke (write -> read)

```bash
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
FILE_NAME="setup/smoke_${STAMP}.md"

curl -sS -X POST "${CONTEXTLATTICE_ORCHESTRATOR_URL}/memory/write" \
  -H "Content-Type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d "{\"projectName\":\"contextlattice\",\"fileName\":\"${FILE_NAME}\",\"content\":\"smoke write ${STAMP}\",\"topicPath\":\"runbooks/setup/smoke\"}" | jq .

curl -sS -X POST "${CONTEXTLATTICE_ORCHESTRATOR_URL}/memory/search" \
  -H "Content-Type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d "{\"project\":\"contextlattice\",\"query\":\"smoke write ${STAMP}\",\"topic_path\":\"runbooks/setup/smoke\",\"include_grounding\":true}" | jq .
```

Expected:
- write returns `ok: true`
- read returns at least one matching result

## 6) DMG-installed users

After first launch, use:
- `~/ContextLattice/setup/agent_contextlattice_instructions.md` (paste into agent/LLM instructions)
- `~/ContextLattice/setup/agent_smoke_write_read.md` (operator write/read verification)

## 7) Per-agent templates

- `docs/public_overview/templates/agents/codex.md`
- `docs/public_overview/templates/agents/claude-code.md`
- `docs/public_overview/templates/agents/opencode.md`
- `docs/public_overview/templates/agents/hermes-agent.md`
- `docs/public_overview/templates/agents/chatgpt-web-desktop.md`
- `docs/public_overview/templates/agents/claude-web-desktop.md`
