# SKILLS.md Template (ContextLattice)

Use this as a reusable skill block for agent frameworks that support skills or tool policy modules.

## ContextLattice Retrieval Skill

### Input Contract
- `mission`: long-horizon purpose and system intent for the current agent run
- `goal`: what the agent is trying to accomplish
- `project`: project namespace (if known)
- `topic_path`: topic namespace (if known)
- `agent_id`: stable agent identifier

### Required Workflow
1. Retrieve before inference:
   - `POST /memory/search` with `include_grounding=true`
2. If task scope is broad, gather dense context:
   - `POST /memory/context-pack`
3. Use profile defaults:
   - `GET /memory/profiles/{agent_id}`
   - Optional bootstrap: `POST /v1/agents/preflight`
   - On preflight responses, ingest and forward `policy_context_package` (mission + objective + goal + policy contract) into downstream agent handoffs.
   - Preserve any returned `format_contract` or `format_contracts` metadata; contract violations are product signals, not wording suggestions.
4. During execution, checkpoint durable decisions:
   - `POST /memory/write`
5. Submit explicit retrieval feedback for learning/rerank:
   - `POST /tools/feedback_submit` (include `idempotencyKey`)
6. Before context compaction/summarization, persist objective continuity and read it back:
   - `contextlattice_agent_orchestration compaction-handoff contextlattice "<objective summary>" runbooks/context-compaction-handoff balanced`
7. Before completion, run one recency retrieval pass:
   - `POST /memory/search` or `POST /memory/context-pack`
8. Set caller timeout to match retrieval mode:
   - `fast`: `25s`
   - `balanced`: `60s`
   - `deep` (or explicit `letta` / `memory_bank`): `75s`

### Source Policy
- Default to mixed retrieval across available stores (for example `topic_rollups`, `qdrant`, `mindsdb`, `letta`, `memory_bank`; pgvector may be present in full/operator stacks).
- Use staged fetch to control latency; allow escalation to slower sources when confidence is low.
- Keep factual numbers as exact copies from retrieved records.
- Expect first deep reads to be slower; repeated equivalent reads should speed up as caches warm.
- Profile keys for common agents:
  - `codex`, `claude-code`, `opencode`, `hermes-agent`
  - `chatgpt-web`, `chatgpt-desktop`, `claude-web`, `claude-desktop`

### Quality Policy
- Evaluate recall quality for release-sensitive flows:
  - `POST /memory/recall/evaluate/saved`
- Track quality and tuning suggestions:
  - `GET /telemetry/recall`
  - `GET /telemetry/recall/monitor`
  - `GET /telemetry/recall/tuning`
- Track boundary contract health:
  - `GET /telemetry/agent-contracts`

### Timeout Troubleshooting
- If a read times out, check the client/tool timeout first.
- Retry the same query once before failing hard, so staged async warming can complete.
