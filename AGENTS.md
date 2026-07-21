# Agent Instructions

ContextLattice is the local memory and capability router for this repo.

## Start
- Use `http://127.0.0.1:8075`.
- Set both aliases when launching tools: `CONTEXTLATTICE_ORCHESTRATOR_URL` and `MEMMCP_ORCHESTRATOR_URL`.
- Before non-trivial work, run `contextlattice_agent_start --soft --compact` or `scripts/agent_hooks/agent_start.sh --soft --compact`.

## Work Loop
1. Ask ContextLattice for scoped context before broad reasoning:
   - narrow: `POST /memory/search`
   - broad/multi-file: `POST /memory/context-pack`
   - helper: `scripts/agent/contextlattice-pack "<task>" --project contextlattice`
2. Read only returned files unless blocked or obviously incomplete.
3. Prefer repo-local conventions and deterministic scripts over repeated inference.
4. Run checks that match the changed surface.
5. Write durable decisions, failures, useful commands, and changed assumptions back:
   - `scripts/agent/writeback --topic-path <path> --file <logical.md> --kind <kind> --stdin`

## Hooks
- Use hooks for mechanical checks: runtime health, git lane, env drift, rebuild gates, leak scans, resource pressure, recall gates, checkpoint readback.
- Keep judgment here: correctness, safety, maintainability, minimal context, and repo-local convention win over style preference.
- Do not paste long runbooks, API docs, examples, or architecture lore into always-loaded instructions. Put them in references/docs or ContextLattice.

## Host Lifecycle Safety
- Treat changes to host supervisors, installers, schedulers, runtime start/stop paths, and recovery policy as host-lifecycle critical.
- Before merge, complete the enforced PR evidence contract, run `scripts/agent/audit-host-supervisor-safety`, the matching failure-injection suite, an installed upgrade-path smoke under an isolated scheduler identity, and at least two real scheduled intervals with identity/restart-count proof.
- Never accept direct source tests as proof of installed behavior, let test jobs reuse an operator's scheduler label, or tag a host-lifecycle change without a tested rollback.

## Compaction
- Before compaction or handoff, write objective state with:
  `scripts/agent_hooks/contextlattice_pre_compaction_write.sh "<objective summary>"`
- After compaction or resume, read it back with:
  `scripts/agent_hooks/contextlattice_post_compaction_read.sh`
- If those wrappers are unavailable, use:
  `python3 scripts/agent_orchestration.py compaction-handoff contextlattice "<objective summary>" runbooks/context-compaction-handoff balanced`

## Boundaries
- Do not duplicate shared rules inside skills; skills should hold activation, compact workflow, hard exceptions, and references.
- Do not load quarantined or vendor-cache skills unless explicitly auditing or promoting them.
- Do not remove safety, verification, or trading-risk controls to save tokens.
- Resolve conflicts toward correctness, safety/security, maintainability, deterministic verification, repo-local convention, then brevity.
