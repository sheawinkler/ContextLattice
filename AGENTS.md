# Agent Instructions

ContextLattice routes local memory and capabilities for this repo.

## Start
- Use `http://127.0.0.1:8075`; set both `CONTEXTLATTICE_ORCHESTRATOR_URL` and `MEMMCP_ORCHESTRATOR_URL` to it.
- Before non-trivial work, run `contextlattice_agent_start --soft --compact` or `scripts/agent_hooks/agent_start.sh --soft --compact`.
- For agent-native sessions, prefer `contextlattice_agent_adapter bootstrap --agent <profile> --project <project>` to track runtime state, handoffs, and reusable prompts.

## Work Loop
1. Retrieve scoped context before broad reasoning:
   - narrow: `POST /memory/search`
   - broad/multi-file: `POST /memory/context-pack`
   - helper: `scripts/agent/contextlattice-pack "<task>" --project contextlattice`
   - session helper: `scripts/agent/contextlattice-session context-package --session-id <session_id> --pretty`
2. Read only returned files unless blocked or incomplete.
3. Prefer repo-local conventions and deterministic scripts over repeated inference.
4. Run checks that match the changed surface.
5. Write durable decisions, failures, commands, and changed assumptions back:
   - `scripts/agent/writeback --topic-path <path> --file <logical.md> --kind <kind> --stdin`
6. Before a major handoff or problem-solving prompt, run `contextlattice_agent_session context-package --session-id <session_id> --pretty` and use the bounded result as the factual scaffold.

## Hooks
- Use hooks for runtime health, git lane, env drift, rebuild, leak, resource, recall, and checkpoint checks.
- Keep judgment here: correctness, safety, maintainability, minimal context, and repo convention beat style preference.
- Do not paste long runbooks, API docs, examples, or architecture lore into always-loaded instructions. Put them in references/docs or ContextLattice.

## Host Lifecycle Safety
- Host supervisors, installers, schedulers, runtime start/stop paths, and recovery policy are host-lifecycle critical.
- Before merge, complete the PR evidence contract; run `scripts/agent/audit-host-supervisor-safety`, matching failure injection, an installed upgrade smoke under an isolated scheduler identity, and at least two real scheduled intervals with identity/restart-count proof.
- Source tests do not prove installed behavior. Never reuse an operator scheduler label or tag a host-lifecycle change without tested rollback.

## Compaction
- Before compaction or handoff: `scripts/agent_hooks/contextlattice_pre_compaction_write.sh "<objective summary>"`.
- After compaction or resume: `scripts/agent_hooks/contextlattice_post_compaction_read.sh`.
- Fallback: `python3 scripts/agent_orchestration.py compaction-handoff contextlattice "<objective summary>" runbooks/context-compaction-handoff balanced`.

## Boundaries
- Do not duplicate shared rules in skills; keep only activation, compact workflow, hard exceptions, and references.
- Do not load quarantined or vendor-cache skills unless explicitly auditing or promoting them.
- Do not remove safety, verification, or trading-risk controls to save tokens.
- Resolve conflicts toward correctness, safety/security, maintainability, deterministic checks, repo convention, then brevity.
