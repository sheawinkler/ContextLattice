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
   - `contextlattice context "<task>" --project <project> --pretty`
   - Use `POST /memory/search` only when the CLI is unavailable or the caller is an app/harness integration.
2. If task scope is broad, gather dense context:
   - Keep using `contextlattice context`; request `--full` only for contract debugging.
3. Use profile defaults:
   - `GET /memory/profiles/{agent_id}`
   - Optional bootstrap: `POST /v1/agents/preflight`
   - On preflight responses, ingest and forward `policy_context_package` (mission + objective + goal + policy contract) into downstream agent handoffs.
   - Preserve any returned `format_contract` or `format_contracts` metadata; contract violations are product signals, not wording suggestions.
4. During execution, checkpoint durable decisions:
   - `contextlattice remember "<checkpoint>" --project <project> --pretty`
5. Before a hard model handoff or problem-solving prompt, package current session state:
   - `contextlattice resume --project <project> --pretty`
   - Prefer the returned compact packet over raw logs.
6. Discover capabilities on demand:
   - `POST /tools/skills_index_search`
   - CLI equivalent: `contextlattice_skills_index search "<task or tool need>" --pretty`
7. Submit explicit retrieval feedback for learning/rerank:
   - `contextlattice correct "<note>" --category useful|wrong|stale|superseded --project <project> --pretty`
8. Before context compaction/summarization, persist objective continuity through the adapter:
   - `contextlattice_agent_adapter handoff --project contextlattice --session-id <session_id> --summary "<objective summary>"`
9. When agent lifecycle changes, report it without confusing it with retrieval progress:
   - `contextlattice_agent_adapter state --project contextlattice --session-id <session_id> --state working|awaiting_user|blocked|done --summary "<why>"`
10. Complete the task and bind its retrieval outcome:
   - `contextlattice finish "<verified result>" --success|--repair|--failure --project <project> --pretty`
11. Set caller timeout to match retrieval mode:
   - `fast`: `25s`
   - `balanced`: `60s`
   - `deep` (or explicit `letta` / `memory_bank`): `75s`

### Source Policy
- Default to mixed retrieval across available stores (for example `topic_rollups`, `qdrant`, `mindsdb`, `letta`, `memory_bank`; pgvector may be present in full/operator stacks).
- Use staged fetch to control latency; allow escalation to slower sources when confidence is low.
- Keep factual numbers as exact copies from retrieved records.
- Expect first deep reads to be slower; repeated equivalent reads should speed up as caches warm.
- Profile keys for common agents:
  - `codex`, `claude-code`, `opencode`, `hermes-agent`, `hermes-ultra`, `omp`, `mercury-agent`, `pi`, `droid`
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
