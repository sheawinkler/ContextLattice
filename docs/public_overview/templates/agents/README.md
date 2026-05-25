# Agent-Specific ContextLattice Templates

These files provide copy-ready instruction blocks for common agents.

- `universal.md` (canonical behavior contract: before/during/after + async + tasks)
- `codex.md`
- `claude-code.md`
- `opencode.md`
- `hermes-agent.md`
- `chatgpt-web-desktop.md`
- `claude-web-desktop.md`

All templates pin the orchestrator endpoint to `http://127.0.0.1:8075` and enforce retrieval-before-inference.
They also enforce the default context-compaction handoff (`compaction-handoff`) so objective state is persisted and immediately re-read around compaction events.
Templates are contract-aware but intentionally light: agents should preserve `format_contract` metadata from ContextLattice, not echo it in every human-facing answer.

Global helper CLI tools are auto-installed by `gmake quickstart` and installer flows:
- `~/.contextlattice/bin/contextlattice_agent_orchestration`
- `~/.contextlattice/bin/contextlattice_search`
- `~/.contextlattice/bin/contextlattice_write`
- `~/.contextlattice/bin/contextlattice_agent_start`
- `~/.contextlattice/bin/contextlattice_checkpoint`

Preferred startup:

```bash
contextlattice_agent_start --soft --compact
```
