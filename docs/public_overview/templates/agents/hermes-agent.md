# Hermes Agent Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `hermes-agent`
- Stable agent id: `hermes_agent`

Session bootstrap:
```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=hermes_agent
export MEMMCP_AGENT_ID=hermes_agent
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
- Run `contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --pretty` to install repo-local instruction files, or paste `docs/public_overview/templates/agents/universal.md` into system instructions when the runtime has no repo instruction file.
- Use `contextlattice_agent_trace --session-id <session-id> --tree` only when advanced provenance debugging is needed.
