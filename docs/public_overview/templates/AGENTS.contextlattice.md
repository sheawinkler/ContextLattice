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
  - `contextlattice_agent_start` (hook-first startup guard)
  - `contextlattice_agent_adapter` (profile bootstrap, context-pack, checkpoint, handoff)
  - `contextlattice_agent_session` (runtime, rollup, context-package, completion)
  - `contextlattice_search` (lifecycle-aware search helper)
  - `contextlattice_write` (checkpoint write helper)
  - `contextlattice_checkpoint` (write + verified readback helper)
  - `contextlattice_skills_index` (capability lookup without loading every skill)

Hook-first default:
```bash
contextlattice_agent_start --soft --compact
```

Agent-session default:
```bash
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent codex --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
contextlattice_agent_session context-package --session-id "$SESSION_ID" --pretty
```

## 3) Checkpoints and Final Recency Pass
- During long tasks, write concise checkpoints via `POST /memory/write`.
- Prefer `contextlattice_checkpoint` so every checkpoint also proves readback.
- Preserve `format_contract`, `format_contracts`, and `policy_context_package` fields returned by ContextLattice in downstream handoffs; do not invent or strip contract metadata.
- When a task outcome is known, submit retrieval quality feedback via `POST /tools/feedback_submit` with an `idempotencyKey`.
- Before final output, run one recency retrieval pass (`/memory/search` or `/memory/context-pack`).
- Before any context-compaction handoff, persist objective state through the adapter:
  - `contextlattice_agent_adapter handoff --project contextlattice --session-id <session_id> --summary "<objective summary>"`
- Before a major model handoff or hard follow-up prompt, package the session:
  - `contextlattice_agent_session rollup --session-id <session_id> --pretty`
  - `contextlattice_agent_session context-package --session-id <session_id> --pretty`
  - Use the returned `reference_prompt`/`context_package` as the bounded factual scaffold instead of raw logs; preserve project, topic/subtopic, and session objective lineage when handing off.
- When the agent needs capabilities but not the whole skills tree, search the active Skills Index:
  - `contextlattice_skills_index search "<task or tool need>" --pretty`
  - Local installs index `${HOME}/.codex/skills` by default; use the returned skill names/paths as pointers, not as permission to load every skill body.
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
- Inspect agent-boundary validation counters when debugging handoff failures:
  - `GET /telemetry/agent-contracts`

## 5) Read Timeout Troubleshooting
- If reads time out, verify the agent/tool timeout is not lower than ContextLattice runtime budget.
- Keep staged fetch enabled to return fast-source results while slower sources continue in parallel.
- Retry the same query once before declaring failure; cache warming improves follow-up latency.
- Use `POST /v1/memory/neighbors` when you need graph-neighbor detail exploration.
