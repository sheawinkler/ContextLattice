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
  - `deep` (or explicit `letta` / `memory_bank`): `75s`
- Note: first deep reads may be slower; repeated calls often return faster after staged fetch and async cache warming.

## 3) Checkpoints and Final Recency Pass
- During long tasks, write concise checkpoints via `POST /memory/write`.
- When a task outcome is known, submit retrieval quality feedback via `POST /tools/feedback_submit` with an `idempotencyKey`.
- Before final output, run one recency retrieval pass (`/memory/search` or `/memory/context-pack`).
- If memory endpoints degrade, continue task execution but report degraded-memory mode.

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
