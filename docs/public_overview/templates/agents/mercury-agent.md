# Mercury Agent Integration Template

Runtime:
- Orchestrator: `http://127.0.0.1:8075`
- Agent profile: `mercury-agent`
- Stable agent id: `mercury_agent`

Use this for Mercury agent sessions. ContextLattice treats Mercury as an external agent harness: Mercury executes; ContextLattice packages context, records lifecycle, stores checkpoints, and preserves handoffs.

Session bootstrap:
```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075
export CONTEXTLATTICE_AGENT_ID=mercury_agent
export MEMMCP_AGENT_ID=mercury_agent
```

Preflight:
```bash
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent mercury-agent --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
contextlattice_agent_adapter context-pack --agent mercury-agent --project contextlattice --session-id "$SESSION_ID" --pretty
contextlattice_agent_session context-package --session-id "$SESSION_ID" --pretty
contextlattice_agent_trace --session-id "$SESSION_ID" --tree
contextlattice_skills_index search "repo conventions testing release" --pretty
```

Behavior contract:
- Run `contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --pretty` to install repo-local instruction files.
- Installer flows may add a managed ContextLattice block to `$HOME/.mercury/soul.md` when Mercury is detected. They do not install Mercury itself.
- Use ContextLattice for scoped memory, context packs, lifecycle state, checkpoints, handoffs, and run traces; keep Mercury as the execution harness.
- No auto-merge or git push unless the user explicitly asks.
