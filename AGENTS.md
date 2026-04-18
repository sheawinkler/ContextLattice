# ContextLattice Agent Contract

This file is the canonical contract for external agents and LLM apps using ContextLattice.

## Endpoint + identity pinning

- Orchestrator base URL: `http://127.0.0.1:8075`
- Set both aliases for compatibility:
  - `CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075`
  - `MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075`
- Use a stable agent identity:
  - `CONTEXTLATTICE_AGENT_ID=<stable_id>`
  - `MEMMCP_AGENT_ID=<stable_id>`

## Preflight before major work

- Profile-aware preflight: `POST /v1/agents/preflight`
- Compatibility alias: `POST /v1/codex/preflight`
- CLI preflight helper (from repo root):
  - `python3 scripts/agent_orchestration.py preflight contextlattice runbooks/codex-integration`
- CLI preflight helper (from any working directory):
  - `REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"`
  - `python3 "$REPO_ROOT/scripts/agent_orchestration.py" preflight contextlattice runbooks/codex-integration`
- Shell wrapper equivalent:
  - `./scripts/agent_orchestration.sh preflight contextlattice runbooks/codex-integration`

## Required operating loop (before/during/after)

1. Before inference: `POST /memory/search` with `include_grounding=true` and scoped `project/topic_path` when known.
2. If scoped search is empty/degraded: run one broader search in the same project.
3. Broad or multi-file tasks: `POST /memory/context-pack`.
4. During execution: write checkpoints with `POST /memory/write`.
5. Before final output: run one recency retrieval (`/memory/search` or `/memory/context-pack`).
6. Graph-neighbor recall: `POST /v1/memory/neighbors` when relationship context is relevant.
7. If `continuation_async` is present: return partial results now, then follow `GET /memory/search/continuations/{token}/events` (or short re-query).
8. Treat copied numbers as verbatim facts; do not rewrite numeric values.

## Task + agent orchestration endpoints

- Submit: `POST /v1/tasks/submit`
- Claim: `POST /v1/tasks/claim`
- Status updates: `POST /v1/tasks/status`
- Queue metrics: `GET /v1/tasks/metrics`
- Compatibility routes also exist under `/agents/tasks/*` for legacy workers.

## Degraded-memory policy

- Continue execution when memory is degraded.
- Explicitly report degraded-memory mode and include continuation token/status if available.

## Copy-ready templates

- Universal contract: `docs/public_overview/templates/agents/universal.md`
- Per-agent templates: `docs/public_overview/templates/agents/`
- Human/operator playbook: `docs/human_agent_instruction_playbook.md`
