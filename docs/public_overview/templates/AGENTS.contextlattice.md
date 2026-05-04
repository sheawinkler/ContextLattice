# AGENTS.md Template (ContextLattice)

Copy this into your own repo as `AGENTS.md` (or merge into your existing instructions).

## 1) Retrieval Before Inference (Required)
- Before planning, coding, or reasoning, retrieve context first from ContextLattice.
- Use `POST /memory/search` with:
  - `query` (task-focused)
  - `project` and/or `topic_path` when known
  - `agent_id` (stable per agent)
  - `include_grounding=true`
- For broad or multi-file tasks, call `POST /memory/context-pack` and reason from that payload.
- Preserve numeric facts exactly as retrieved; do not alter copied numbers.

## 2) Profile-Aware Defaults
- Use stable `agent_id` values for each agent persona.
- Configure retrieval defaults with `GET/PUT /memory/profiles/{agent_id}`:
  - sources
  - retrieval mode (`fast`, `balanced`, `deep`)
  - escalation and query expansion policy
  - default project/topic scope
- Set caller timeout by retrieval mode:
  - `fast`: `25s`
  - `balanced`: `60s`
  - `deep` (blocking reads): `75s`
- Explicit source selection does not force blocking.
  - Use `blocking=true` (or `sync_slow_sources=true`) only when you need blocking slow-source completion.
- Note: first deep reads may be slower; repeated calls often return faster after staged fetch and async cache warming.
- Optional profile-aware preflight endpoint:
  - `POST /v1/agents/preflight`
  - `POST /v1/codex/preflight` (compatibility alias)
- Common profile keys:
  - `codex`, `claude-code`, `opencode`, `hermes-agent`
  - `chatgpt-web`, `chatgpt-desktop`, `claude-web`, `claude-desktop`
- Global helper CLIs (auto-installed by quickstart/installers):
  - `contextlattice_agent_orchestration` (preflight/task helpers)
  - `contextlattice_search` (lifecycle-aware search helper)
  - `contextlattice_write` (checkpoint write helper)

## 3) Checkpoints and Final Recency Pass
- During long tasks, write concise checkpoints via `POST /memory/write`.
- When a task outcome is known, submit retrieval quality feedback via `POST /tools/feedback_submit` with an `idempotencyKey`.
- Before final output, run one recency retrieval pass (`/memory/search` or `/memory/context-pack`).
- If memory endpoints degrade, continue task execution but report degraded-memory mode.
- Optional tool-call key split:
  - `CONTEXTLATTICE_ORCHESTRATOR_API_KEY` for orchestrator/admin tasks.
  - `CONTEXTLATTICE_WORKER_API_KEY` for worker tasks when role-split is enabled.

## 4) Recall Quality Gates
- For release-sensitive work, run saved recall evaluation:
  - `POST /memory/recall/evaluate/saved`
- Monitor quality over time:
  - `GET /telemetry/recall`
  - `GET /telemetry/recall/monitor`
  - `GET /telemetry/recall/tuning`

## 5) Read Timeout Troubleshooting
- If reads time out, verify the agent/tool timeout is not lower than ContextLattice runtime budget.
- Keep staged fetch enabled to return fast-source results while slower sources continue in parallel.
- Retry the same query once before declaring failure; cache warming improves follow-up latency.
- Use `POST /v1/memory/neighbors` when you need graph-neighbor detail exploration.
