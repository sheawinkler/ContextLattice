# Codex Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `codex`
- Stable agent id: `codex_gpt5`

Session bootstrap:
```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=codex_gpt5
export MEMMCP_AGENT_ID=codex_gpt5
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
- Codex SessionStart hook template: `config/codex/contextlattice_agent_start.sh`
- Install Codex hooks: `scripts/install_global_agent_tools.sh --install-codex-hooks`
- Verify hooks and repo instructions: `contextlattice_adopt status --pretty && contextlattice_doctor --agents codex --skip-provider-smoke --pretty`
- Use `contextlattice_agent_trace --session-id <session-id> --tree` only when advanced provenance debugging is needed.
