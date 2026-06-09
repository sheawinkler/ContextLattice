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
| `contextlattice_agent_adapter` | Universal agent lifecycle adapter for bootstrap, context-pack, checkpoint, handoff, event, and completion. |
| `contextlattice_agent_runtime_proof` | One-command live proof that bootstrap, scoped recall, checkpoint, handoff, completion, status, and runtime telemetry work end to end. |
| `contextlattice_source_backfill` | Bounded import from files, JSONL, JSON, CSV, SQLite, DuckDB/Parquet, or Postgres into `/memory/write`, with optional graph edge repair. |
| `contextlattice_codex_session_store_doctor` | Checks Codex transcript storage for symlink, external-volume, cloud-folder, TCC, and read/write traps. |
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
contextlattice_agent_adapter bootstrap --agent codex --project contextlattice --pretty
contextlattice_agent_runtime_proof --pretty
```

This creates or recovers a ContextLattice-owned session, returns bounded exports,
and emits a contract-valid `universal_agent_adapter_response.v1`. For Codex hook
startup, `contextlattice_agent_start --soft --compact` still runs:
1. Codex session-store doctor
2. resource pressure sampler
3. git lane guard
4. OrbStack/host-forward guard
5. native endpoint smoke
6. recall monitor seed
7. agent policy/context pack retrieval

`--soft` is intentional for session startup: agents should learn current state
without blocking if the local app is restarting.

## Strict release sequence

```bash
contextlattice_git_lane_guard --branch main --upstream origin/main --require-clean --require-synced
contextlattice_branch_lane_guard --lane public --ref refs/remotes/public/main
contextlattice_branch_lane_guard --lane public-paid --ref refs/remotes/public-paid/main
contextlattice_runtime_env_guard --strict
contextlattice_rust_rebuild_gate --check
contextlattice_recall_quality_gate
contextlattice_public_leak_guard --mode all --ref refs/remotes/public/main --public
contextlattice_public_leak_guard --mode all --ref refs/remotes/public-paid/main
```

If any strict gate fails, stop and fix the underlying condition. Do not explain
past the gate unless the user asks for triage.

Canonical production branch labels:
- `origin/main`: premium-premium / sigma development and testing.
- `public/main`: public production, from remote `public` branch `main`.
- `public-paid/main`: paid production, from remote `public-paid` branch `main`.

Do not report `origin/public/main` or `origin/public-paid/main` as production
branches. Those names are local fetch-namespace aliases when `origin` fetches
every remote branch from the private repository.

Recommended local fetch namespace:

```bash
git config --unset-all remote.origin.fetch
git config --add remote.origin.fetch '+refs/heads/*:refs/remotes/origin/*'
git config --add remote.origin.fetch '^refs/heads/public/main'
git config --add remote.origin.fetch '^refs/heads/public-paid/main'

git config --unset-all remote.public.fetch
git config --add remote.public.fetch '+refs/heads/*:refs/remotes/public/*'
git config --add remote.public.fetch '^refs/heads/public/main'

git config --unset-all remote.public-paid.fetch
git config --add remote.public-paid.fetch '+refs/heads/public-paid/main:refs/remotes/public-paid/main'
```

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

## Codex session storage

Default Codex transcript storage under local `~/.codex/sessions` is the safe
path. ContextLattice does not require users to move it.

External or symlinked transcript storage is advanced mode. It can depend on the
external volume being mounted, macOS privacy/TCC state, and whether the process
that launched Codex has access to the real target path. The same risk applies
when sessions live under Desktop, Documents, Downloads, iCloud, Dropbox, Google
Drive, OneDrive, or another cloud-sync/TCC-managed location.

Run the doctor when Codex resume looks like transcript corruption or when a
machine uses external session storage:

```bash
contextlattice_codex_session_store_doctor --pretty
```

The doctor resolves `~/.codex/sessions`, checks read/write/traverse access,
warns when the real path crosses `/Volumes/*` or a cloud/TCC-managed folder,
samples transcript readability, and prints the exact failing path with a
suggested fix. Warnings do not fail the aggregate agent context audit; hard
access failures do.

## Context compaction

Codex 0.130.0+ exposes `PreCompact` and `PostCompact` hook events. The installer
wires those events to ContextLattice wrappers, and
`scripts/agent/audit-compaction-hooks` verifies both the repo template and live
`~/.codex/hooks.json`. The installer also refreshes the matching
`~/.codex/config.toml` hook trust hashes so new sessions do not need repeated
manual hook review when commands are unchanged.

```bash
contextlattice_pre_compaction_write "current objective, blockers, next actions"
contextlattice_post_compaction_read
```

Codex compact hook stdout is intentionally stricter than SessionStart or tool
hooks. `PreCompact` and `PostCompact` command output must be universal hook
fields only: `continue`, `suppressOutput`, `systemMessage`, and `stopReason`.
Do not emit `hookSpecificOutput`, `ok`, `hook`, raw ContextLattice read/write
JSON, or any env-file stdout on these compact hooks. The wrappers keep the rich
handoff payload in ContextLattice and return only the bounded Codex-compatible
envelope to stdout.

The pre/post compaction hooks derive a compact handoff payload with
`scripts/agent/compaction-handoff-payload`. The payload records session id, cwd,
branch, changed files, commands, blockers, and next action when the hook input
contains them. Post-compaction reads use those terms first so resume context is
scoped to the interrupted session instead of broad historical notes.

Compaction handoff prompts must stay model-provider neutral. The source of truth
is `config/model_compat/compaction_prompt_contract.json`, not any Python hook
wrapper. The contract requires plain UTF-8 text that is safe as ordinary chat
message content or as a single completion prompt, with no vendor-specific chat
template tokens, role-channel envelope assumptions, tool/function-call envelope
requirements, or markdown-fence dependency. The compatibility gate covers the
online model families tracked in the contract plus local runtimes such as
OpenAI-compatible endpoints, Ollama, llama.cpp, vLLM, LM Studio, and MLX. Go and
Rust tests load the same contract so provider/runtime compatibility is not
defined solely by the Codex hook script.

Context-pack shape is guarded by:

```bash
scripts/agent/audit-agent-output-contracts
scripts/agent/audit-agent-boundary-live
scripts/agent/audit-context-pack-schema
scripts/agent/audit-context-overflow-recovery
scripts/agent/eval-skill-policy
scripts/agent/audit-compaction-hooks
```

Agent-facing boundary payloads use the shared registry at
`config/agent_contracts/agent_output_contracts.json`. Preflight responses include
the active contract IDs in `format_contracts`, and each `policy_context_package`
includes `format_contract.schema_id=policy_context_package.v1` plus validation
status. Go and Python loaders read the same registry so agents receive the
contract ContextLattice expects them to satisfy rather than relying on prompt
memory.

Provider/context overflow errors must be treated as recoverable before they are
shown to a user. Use `scripts/agent/context-overflow-recovery` to classify
provider-style context failures, deterministically reduce oversized input
arrays, preserve function/tool call-output invariants, and emit
`context_overflow_recovery.v1` with `status=auto_compacted`,
`recovered_from=context_overflow`, and `user_visible_error=false`. The audit gate
uses provider-style error fixtures and oversized Responses-style input arrays so
raw errors such as `array_above_max_length` or context-length failures do not
become the user-facing contract.

Before a release, run the live boundary canary against the local gateway:

```bash
scripts/agent/audit-agent-boundary-live --pretty
```

The canary hits `/memory/context-pack`, `/tools/context_pack`,
`/v1/agents/preflight`, `/v1/codex/preflight`, and compact hook stdout wrappers
with adversarial oversized inputs. By default it exercises Codex, Claude Code,
Hermes, and Pi agent profiles through the generic preflight route. It validates
the shared contracts and fails if raw provider-overflow-shaped text leaves the
product boundary.

Memory graph observability is available from the gateway at
`GET /telemetry/memory/graph` and from the terminal:

```bash
scripts/agent/memory-graph-observe
```

Graph quality repair is a bounded maintenance lane over that telemetry:

```bash
scripts/agent/memory-graph-quality --all-projects --pretty
scripts/agent/memory-graph-quality --project context-lattice-private --write --confirm-repair context-lattice-private --pretty
make memory-graph-quality-install
```

The job scores isolated docs, stale inferred edges, sparse density, and
over-connected anchors. It always runs a dry-run preflight before writes, caps
candidate and write counts, and only writes when explicit confirmation is
provided. The launchd runner defaults to dry-run mode; set
`CONTEXTLATTICE_GRAPH_QUALITY_WRITE=1` before install to enable scheduled
bounded repairs.

Project bootstrap for a new repo should stay bounded and curated. Use:

```bash
scripts/agent/project-bootstrap-memory --repo /path/to/repo --project project-name --write --apply-edges
```

This writes compact overview/code-map/next-work notes from high-signal files
only, then runs memory edge backfill for that project. It is not whole-repo raw
ingestion.

General external data backfill uses the same bounded write path and stays
dry-run by default:

```bash
scripts/agent/source-backfill-memory --source jsonl --path exports/tasks.jsonl --project project-name --pretty
scripts/agent/source-backfill-memory --source sqlite --path app.db --table notes --project project-name --pretty
scripts/agent/source-backfill-memory --source parquet --path warehouse/events.parquet --project project-name --pretty
scripts/agent/source-backfill-memory --source postgres --dsn "$DATABASE_URL" --query "select id,title,body from notes limit 100" --project project-name --pretty
scripts/agent/source-backfill-memory --source jsonl --path exports/tasks.jsonl --project project-name --write --confirm-write project-name --apply-edges --pretty
```

Supported adapters are files/directories, JSONL, JSON, CSV, SQLite, DuckDB,
Parquet via DuckDB, and Postgres via optional `psycopg`. The tool caps records,
row bytes, document bytes, total bytes, and structured-list items; secret-like
fields are redacted by default.

Agent ContextLattice wrappers retry and fail non-zero by default when reads or
writes cannot complete. A compact JSON failure replaces Python tracebacks, but
it is still a failure. Use context-pack `--soft` only for non-critical startup
orientation, not durable writes or required context retrieval. Writeback has no
soft success path.

If agent writeback cannot reach ContextLattice after retries, the wrapper writes
the exact payload to a host-local durable queue under
`~/.contextlattice/writeback_queue`, returns `persisted: false`, and exits
non-zero. Drain it after recovery with:

```bash
scripts/agent/drain-writeback-queue
```

OrbStack repair has two layers:

```bash
scripts/orbstack_self_heal.sh run-once --event manual
scripts/install_orbstack_self_heal_runner.sh
```

Agent read/write failures trigger `orbstack_self_heal.sh run-once` in the
background. The launchd runner is the periodic fallback for cases where no agent
operation happens while the VM is wedged.
