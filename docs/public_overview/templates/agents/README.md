# Agent-Specific ContextLattice Templates

These files provide copy-ready instruction blocks for common agents.

- `universal.md` (canonical behavior contract: before/during/after + async + tasks)
- `codex.md`
- `claude-code.md`
- `opencode.md`
- `hermes-agent.md`
- `omp.md`
- `mercury-agent.md`
- `pi.md`
- `droid.md`
- `chatgpt-web-desktop.md`
- `claude-web-desktop.md`

All templates pin the orchestrator endpoint to `http://127.0.0.1:8075` and enforce retrieval-before-inference.
They also enforce the default context-compaction handoff (`compaction-handoff`) so objective state is persisted; post-compaction readback is bounded and used for recovery, not prompt filler.
Templates are contract-aware but intentionally light: agents should preserve `format_contract` metadata from ContextLattice, not echo it in every human-facing answer.

Global helper CLI tools are auto-installed by `gmake quickstart` and installer flows:
- `$HOME/.contextlattice/bin/contextlattice_adopt`
- `$HOME/.contextlattice/bin/contextlattice_agent_adapter`
- `$HOME/.contextlattice/bin/contextlattice_search`
- `$HOME/.contextlattice/bin/contextlattice_write`
- `$HOME/.contextlattice/bin/contextlattice_agent_start`
- `$HOME/.contextlattice/bin/contextlattice_checkpoint`
- `$HOME/.contextlattice/bin/contextlattice_agent_session`
- `$HOME/.contextlattice/bin/contextlattice_async_inbox_drain`
- `$HOME/.contextlattice/bin/contextlattice_async_inbox_hook`
- `$HOME/.contextlattice/bin/contextlattice_agent_trace`
- `$HOME/.contextlattice/bin/contextlattice_agent_adoption_proof`
- `$HOME/.contextlattice/bin/contextlattice_agent_runtime_doctor`
- `$HOME/.contextlattice/bin/contextlattice_skills_index`

Preferred startup:

```bash
contextlattice_adopt status --pretty
contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --pretty
contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --check --pretty
BOOTSTRAP_JSON="$(contextlattice_agent_adapter bootstrap --agent codex --project contextlattice)"
SESSION_ID="$(printf '%s' "$BOOTSTRAP_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["session_id"])')"
contextlattice_agent_session context-package --session-id "$SESSION_ID" --pretty
contextlattice_async_inbox_drain --session-id "$SESSION_ID"
contextlattice_async_inbox_hook --session-id "$SESSION_ID"
contextlattice_agent_trace --session-id "$SESSION_ID" --tree
contextlattice_skills_index search "browser automation" --pretty
contextlattice_agent_adoption_proof --skip-provider-smoke --progress --pretty
```

For a new machine, account, or custom agent, start with `contextlattice_adopt status --pretty`, then run `contextlattice_agent_adapter profiles --pretty` to inspect the supported Go-native profiles.

`contextlattice_skills_index` searches active configured skill roots such as `${HOME}/.codex/skills`; quarantined/vendor skill discovery remains separate.
