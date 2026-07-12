# OMP Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `omp`
- Stable agent id: `omp_agent`

Use this for Oh My Pi / OMP terminal-agent sessions. OMP can read repo-local `AGENTS.md` conventions, so `contextlattice_adopt integrate` writes the OMP profile into the shared managed `AGENTS.md` block.

Session bootstrap:
```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=omp_agent
export MEMMCP_AGENT_ID=omp_agent
```

Primary workflow:
```bash
contextlattice doctor --pretty
contextlattice context "current task" --project contextlattice --pretty
contextlattice resume --project contextlattice --pretty
contextlattice remember "checkpoint summary" --project contextlattice --pretty
contextlattice finish "verified result" --success --project contextlattice --pretty
contextlattice_skills_index search "repo conventions testing release" --pretty
```

Behavior contract:
- Run `contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --pretty` to install repo-local instruction files.
- Installer flows may add a managed ContextLattice block to `$HOME/.omp/agent/AGENTS.md` when OMP is detected. They do not install OMP itself.
- Use ContextLattice for scoped memory, context packs, lifecycle state, checkpoints, handoffs, and run traces; keep OMP as the execution harness.
- No auto-merge or git push unless the user explicitly asks.
