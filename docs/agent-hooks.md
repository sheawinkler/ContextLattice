# ContextLattice Agent Hooks

ContextLattice ships deterministic agent hooks so agents spend fewer tokens
remembering mechanical rules and more tokens reasoning about the task.

## Rule

Use hooks for stable pass/fail mechanics. Keep judgment in `AGENTS.md`.

Good hook targets:
- environment setup
- health checks
- git branch/status gates
- rebuild-required markers
- secret/path leak checks
- resource pressure checks
- checkpoint/write/readback verification

Bad hook targets:
- ambiguous product judgment
- subjective design critique
- trading or launch decisions without evidence review
- anything that needs human approval

## Global install

```bash
scripts/install_global_agent_tools.sh --install-codex-hooks
```

Optional machine-local hook policy belongs outside git:

```bash
cat > ~/.contextlattice/agent_hooks.env <<'EOF'
export CONTEXTLATTICE_EXTERNAL_DATA_ROOT="/Volumes/<external-data-volume>"
export CONTEXTLATTICE_PUBLIC_FORBIDDEN_PATH_RE='(/Volumes/<external-data-volume>|/Users/<local-user>|<private-repo-name>)'
EOF
chmod 600 ~/.contextlattice/agent_hooks.env
```

Installed commands:

| Command | Purpose |
| --- | --- |
| `contextlattice_agent_start` | Compact startup guard for agents. |
| `contextlattice_preflight_hook` | ContextLattice preflight wrapper. |
| `contextlattice_checkpoint` | Write checkpoint and verify readback. |
| `contextlattice_git_lane_guard` | Branch, upstream, clean-tree, sync checks. |
| `contextlattice_branch_lane_guard` | Private/public/public-paid lane hygiene. |
| `contextlattice_rust_rebuild_gate` | Detect Rust changes and enforce full rebuild. |
| `contextlattice_runtime_env_guard` | Detect stale/conflicting env override drift. |
| `contextlattice_recall_quality_gate` | Recall eval/telemetry pre-release gate. |
| `contextlattice_resource_pressure_guard` | Host disk/RAM/container runtime pressure sampler. |
| `contextlattice_orbstack_forward_guard` | Docker/OrbStack and 8075 host-forward repair guard. |
| `contextlattice_native_endpoint_smoke` | Fast smoke for critical go-native routes after restart/redeploy. |
| `contextlattice_recall_monitor_seed` | Seed recall monitor snapshot when cold so tuning has live samples. |
| `contextlattice_public_leak_guard` | Secret, private path, and machine-local path scanner. |
| `contextlattice_agent_policy_pack` | Compact mission/objective/goal + retrieval package. |
| `contextlattice_command_output_budget` | Bounded command output with full artifact capture. |
| `contextlattice_pre_compaction_write` | Persist objective state before compaction/handoff. |
| `contextlattice_post_compaction_read` | Read objective state after compaction/resume. |

## Recommended startup sequence

```bash
contextlattice_agent_start --soft --compact
```

This runs:
1. resource pressure sampler
2. git lane guard
3. OrbStack/host-forward guard
4. native endpoint smoke
5. recall monitor seed
6. agent policy/context pack retrieval

`--soft` is intentional for session startup: agents should learn current state
without blocking if the local app is restarting.

## Strict release sequence

```bash
contextlattice_git_lane_guard --branch main --upstream origin/main --require-clean --require-synced
contextlattice_runtime_env_guard --strict
contextlattice_rust_rebuild_gate --check
contextlattice_recall_quality_gate
contextlattice_public_leak_guard --mode changed --base origin/main --public
```

If any strict gate fails, stop and fix the underlying condition. Do not explain
past the gate unless the user asks for triage.

## Checkpoint pattern

```bash
printf '%s\n' 'short factual checkpoint' | \
  contextlattice_checkpoint \
    --project contextlattice \
    --topic-path runbooks/codex-integration \
    --file notes/codex/checkpoint.md \
    --stdin
```

## Codex hook config

`--install-codex-hooks` installs:
- `~/.codex/hooks/contextlattice_agent_start.sh`
- `~/.codex/hooks/contextlattice_pre_compaction_write.sh`
- `~/.codex/hooks/contextlattice_post_compaction_read.sh`
- a `SessionStart` entry in `~/.codex/hooks.json`
- `PreCompact` and `PostCompact` entries in `~/.codex/hooks.json`

The Codex startup hook runs the same `contextlattice_agent_start --soft --compact`
path. This makes Codex session start consistent with the public ContextLattice
agent hook pack.
The installed Codex hook timeout is 90s so OrbStack/container startup does not
false-fail during normal warmup.

`caveman_mode.sh` is intentionally not installed as a startup hook. Use the
`caveman` skill only when the user asks for terse/low-token output.

Hook trust is deterministic. Run this after changing Codex hook JSON or managed
hook commands:

```bash
scripts/agent/audit-codex-hook-trust --repair
```

## Context compaction

Codex 0.130.0 exposes `PreCompact` and `PostCompact` hook events. The installer
wires those events to ContextLattice wrappers, and
`scripts/agent/audit-compaction-hooks` verifies both the repo template and live
`~/.codex/hooks.json`. The installer also refreshes the matching
`~/.codex/config.toml` hook trust hashes so new sessions do not need repeated
manual hook review when commands are unchanged.

```bash
contextlattice_pre_compaction_write "current objective, blockers, next actions"
contextlattice_post_compaction_read
```

The pre/post compaction hooks derive a compact handoff payload with
`scripts/agent/compaction-handoff-payload`. The payload records session id, cwd,
branch, changed files, commands, blockers, and next action when the hook input
contains them. Post-compaction reads use those terms first so resume context is
scoped to the interrupted session instead of broad historical notes.

Context-pack shape is guarded by:

```bash
scripts/agent/audit-context-pack-schema
scripts/agent/eval-skill-policy
```

Agent ContextLattice wrappers retry and fail non-zero by default when reads or
writes cannot complete. A compact JSON failure replaces Python tracebacks, but
it is still a failure. Use context-pack `--soft` only for non-critical startup
orientation, not durable writes or required context retrieval. Writeback has no
soft success path.
