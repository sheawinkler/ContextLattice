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
contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --pretty
contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --check --pretty
contextlattice_doctor --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --skip-provider-smoke --pretty
```

Optional machine-local hook policy belongs outside git:

```bash
cat > ~/.contextlattice/agent_hooks.env <<'EOF'
export CONTEXTLATTICE_EXTERNAL_DATA_ROOT="/Volumes/<external-data-volume>"
export CONTEXTLATTICE_PUBLIC_FORBIDDEN_PATH_RE='(/Volumes/<external-data-volume>|/Users/<local-user>|<private-repo-name>)'
EOF
chmod 600 ~/.contextlattice/agent_hooks.env
```

Shell-only `export CONTEXTLATTICE_REPO_ROOT=...` is not enough for Codex hooks;
persist it in `~/.contextlattice/agent_hooks.env` or reinstall from the intended
checkout so the installer writes it.

The installer copies a minimal Python hook runtime by default. That runtime is
not the optional development Python tool suite; it is the compact glue used by
SessionStart, PreCompact, and PostCompact for session recovery, handoff payload
shaping, and provider-neutral compaction prompts.

Installed commands:

| Command | Purpose |
| --- | --- |
| `contextlattice` | Primary compact workflow: `context`, `resume`, `remember`, `finish`, `correct`, `utility`, and `doctor`. |
| `contextlattice_agent_start` | Compact startup guard for agents. |
| `contextlattice_agent_adapter` | Universal agent lifecycle adapter for bootstrap, context-pack, checkpoint, handoff, state, event, and completion. |
| `contextlattice_agent_discover` | Best-effort local agent discovery for profile authority, process evidence, hook evidence, repo instruction evidence, and lifecycle explanations. |
| `contextlattice_agent_session` | Session lifecycle, rollup, context-package, trace, runtime, and cleanup CLI. |
| `contextlattice_packet_reconstruct` | Verify a digest-bound `agent_packet_delta.v1` against its trusted full base and emit the reconstructed `agent_packet.v1`. |
| `contextlattice_async_inbox_drain` | Bounded async continuation inbox drain; emits unseen terminal steering after normal CLI boundaries. |
| `contextlattice_async_inbox_hook` | Fail-open hook entrypoint for runtimes with post-tool or post-command hooks. |
| `contextlattice_agent_trace` | Contract-valid run trace, exportable run card, and optional `--proof` timeline linking context, action, correction, verification, outcome, and learning. |
| `contextlattice_adopt` | Zero-friction adoption front door for status, install guidance, proof, profiles, and CI-style adoption scenarios. |
| `contextlattice_doctor` | One-command adoption proof for local readiness, lifecycle proof, and trace-visible run shaping evidence. |
| `contextlattice_agent_runtime_proof` | One-command live proof that bootstrap, scoped recall, checkpoint, handoff, context-package, completion, status, and runtime telemetry work end to end. |
| `contextlattice_agent_adoption_proof` | Matrix proof that configured agent profiles can use the same memory lifecycle and expose skills/context/session/graph/handoff evidence. |
| `contextlattice_agent_runtime_doctor` | Local helper, hook, wrapper, and gateway drift audit. |
| `contextlattice_memory_topology` | Memory topology audit for base/default lanes, full backend fabric, partition keys, clusters, and graph health. |
| `contextlattice_memory_graph_repair` | Audit or apply identity-first hot-corpus edges with dry-run default, exact project confirmation, and a hard per-run write cap. |
| `contextlattice_memory_graph_efficacy` | Generate explicit graph-neighbor holdouts and require healthy direct recall plus positive, hydrated graph contribution. |
| `contextlattice_skills_index` | Skills Index search CLI for discovering capabilities without bloating startup context. |
| `contextlattice_retrieval_plan` | Advisor-only evidence obligations, source/query plan, token allocation, graph expansion advice, and marginal-value stop conditions. |
| `contextlattice_retrieval_governance` | Entitled receipt, causal-bridge, counterfactual, reputation, regression, and adversarial-defense policy governance; it never executes retrieval or disables public defenses. |
| `contextlattice_policy_lab` | Primary Policy Laboratory CLI for simulation, scoped cards, promotion evidence, reversible lifecycle, contradiction, storage-temperature, and governance status. |
| `contextlattice_agent_fit` | Primary Agent Fit CLI for resumable steering, advisory runner/model selection, effective profiles, and explicit-use context preparation. |
| `contextlattice_continuity_zero` | Primary zero-entry CLI: select one unambiguous live objective and bind its packet, checkpoint, Agent Fit profile, preparation, repository commit, provenance, risks, and next move into one path-free manifest. |
| `contextlattice_aggregate_signal` | Primary Aggregate Signal CLI for local preview, explicit opt-in queueing, cohort-suppressed reports, privacy accounting, and immediate opt-out. |
| `contextlattice_agent_tools portable-continuation` | Primary Portable Continuation CLI family for signed grants, provenance-preserving imports, encrypted manifests, dry-run reconciliation, and bounded status. |
| `contextlattice_claim_write` | Persist or revise a structured temporal claim with provenance, validity, contradiction, supersession, causality, branch, and commit identity. |
| `contextlattice_claim_query` | Query current or historical structured claims without flattening supersession or contradiction. |
| `contextlattice_continuity_reconcile` | Resolve one stable task identity exact-first, keep its execution lane separate, and abstain on semantic ambiguity; merge and split require explicit operator attribution and reason. |
| `contextlattice_objective_transition` | Append a typed, actor-attributed objective transition without overwriting prior state. |
| `contextlattice_objective_graph` | Replay objective lineage and typed links as of now or an explicit timestamp. |
| `contextlattice_decision_change` | Record or query evidence-triggered decision changes with confidence deltas and bounded rationale, never hidden reasoning. |
| `contextlattice_synthesis_pack_v2` | Proof-carrying synthesis with claim-level support, opposition, temporal state, confidence decomposition, and missing-proof disclosure. |
| `contextlattice_policy_candidate` | Derive a bounded advisory context-policy candidate from calibration-eligible outcomes. |
| `contextlattice_policy_evaluate` | Record one shadow/canary lifecycle evaluation without mutating public runtime policy. |
| `contextlattice_policy_status` | Inspect policy candidates, phases, evaluations, and bounded-ledger health. |
| `contextlattice_skill_draft` | Convert repeated verified workflow-run evidence into an inactive skill draft. |
| `contextlattice_skill_evaluate` | Test a draft against independent holdouts with training-leakage rejection. |
| `contextlattice_skill_export` | Export a passing skill only after explicit named human approval; never auto-installs it. |
| `contextlattice_skill_retire` | Persist an immutable terminal tombstone for an inactive draft; never deletes evidence or mutates runtime. |
| `contextlattice_skill_foundry_status` | Inspect Skill Foundry drafts, evaluations, exports, retirements, and bounded-ledger health. |
| `contextlattice_passport_export` | Compile and sign a bounded proof-carrying context manifest; `--output` avoids repeating the artifact in agent context. |
| `contextlattice_passport_verify` | Verify Passport digest, Ed25519 signature, validity window, and schema. |
| `contextlattice_passport_diff` | Compare signed Passport revisions without inference. |
| `contextlattice_passport_replay` | Render a validated replay request without executing imported content. |
| `contextlattice_passport_import` | Explicitly persist a verified Passport with conflict-preserving lineage reconciliation. |
| `contextlattice_passport_status` | Inspect Passport identity, lineages, conflicts, and bounded storage. |
| `contextlattice_mesh_identity` | Return only the local public Ed25519/age identity; private keys never leave the data volume. |
| `contextlattice_mesh_grant` | Create, list, or revoke signed project-scoped recipient grants. |
| `contextlattice_mesh_export` | Encrypt a stored Passport to explicit age X25519 grants; ContextLattice performs no delivery. |
| `contextlattice_mesh_import` | Decrypt and verify an envelope, dry-run by default, then apply only with `--apply`. |
| `contextlattice_mesh_status` | Inspect bounded grants, revocations, receipts, limits, and the no-transport boundary. |
| `contextlattice_source_backfill` | Optional development helper, installed with `--include-dev-python-tools`, for bounded import from files, JSONL, JSON, CSV, SQLite, DuckDB/Parquet, or Postgres. |
| `contextlattice_codex_session_store_doctor` | Optional development helper, installed with `--include-dev-python-tools`, for Codex transcript storage checks. |
| `contextlattice_runner_quality` | Primary CLI for bounded runner-quality telemetry and advisor-only runner recommendations. |
| `contextlattice_recall_quality_eval` | Primary CLI for saved recall quality, same-snapshot ablation, review-only regression derivation, and advisory evidence reputation. |
| `contextlattice_preflight_hook` | ContextLattice preflight wrapper. |
| `contextlattice_checkpoint` | Write checkpoint and verify readback. |
| `contextlattice_git_lane_guard` | Branch, upstream, clean-tree, sync checks. |
| `contextlattice_branch_lane_guard` | Private/public/public-paid lane hygiene. |
| `contextlattice_rust_rebuild_gate` | Detect Rust changes and enforce full rebuild. |
| `contextlattice_runtime_env_guard` | Detect stale/conflicting env override drift. |

Pi and Droid runner adapters are repo-local task-worker internals under `scripts/agent_runners/`. ContextLattice reports install hints and readiness, but it does not install or require third-party Pi/Droid CLIs for quickstart.

OMP and Mercury are instruction-hook integrations, not bundled harnesses. Installer flows upsert a bounded managed ContextLattice block into detected user instruction files:

- OMP: `$HOME/.omp/agent/AGENTS.md`
- Mercury: `$HOME/.mercury/soul.md`

Use `scripts/install_global_agent_tools.sh --no-agent-hooks` or `contextlattice_adopt install --no-install-agent-hooks` to opt out. These hooks teach the detected agent the compact `contextlattice context/resume/remember/finish/correct/doctor` workflow; they do not install OMP or Mercury binaries.
Runner adapter completions write compact `runner_quality_sample.v1` rows when task-agent workers can access the ledger. Inspect them with primary CLI command `contextlattice_runner_quality --pretty` or repo-local development fallback `scripts/agent/runner-quality --pretty`.
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

### Portable Continuation

Use `contextlattice_agent_tools portable-continuation` as the canonical
interface. POST operations require an owner-only `--payload-file`; use
`--output` for the encrypted envelope so ciphertext does not consume agent
context. `manifest-reconcile` accepts that artifact through `--envelope-file`.
The gateway verifies and records contracts only: an operator-chosen external
adapter owns delivery and import execution.

### Continuity Zero

Run `contextlattice_continuity_zero --project <project> --agent <harness> --output continuity-zero.json --pretty` from the repository you are resuming. The CLI derives repository, branch, and commit identity with argv-safe Git calls, sends no local path to the gateway, and writes the optional artifact owner-only. It returns `ready` only when one fresh, non-terminal session matches the project, agent, harness, repository, and commit; ambiguity or stale/mismatched provenance abstains or rejects instead of guessing.

The public route is advisory and never creates a session, runs a model, dispatches a runner, mutates a worktree, or transports the manifest. Entitled automation records only explicit `push` or `workspace_prepare` intents plus external-adapter receipts. ContextLattice remains the control and proof boundary; the selected adapter remains the executor.

### Aggregate Signal

Start with `contextlattice_aggregate_signal preview --metric <metric> --value <number> --pretty`. Preview is local, does not persist, and performs no network call. Queueing requires both `--opt-in` and a fresh bounded `--nonce`; `status` exposes the local 90-day privacy composition, and `opt-out --confirm` deletes unreleased contributions, rotates the local commitment secret, and stops future contribution without claiming that an already released aggregate can be subtracted.

Only allowlisted, clipped numerical or categorical sufficient statistics enter the queue. Raw memory, prompts, embeddings, file paths, project names, exact timestamps, and stable installation identifiers are rejected recursively. Reports suppress cohorts below 20, cap each release at epsilon 0.25 and the rolling 90-day budget at epsilon 2.0, and remain explicitly research-only with no formal privacy claim until independent attack and utility reviews pass. Operator and Enterprise artifacts may add signed-credential cohort governance; ContextLattice never installs or enables external transport for it.

### Verified Skill Evolution

Use `contextlattice_agent_tools skill-evolution` as the canonical interface for
`reusable-candidate`, explicit `foundry-handoff`, and
`retirement-candidate`. Every operation reads one bounded owner-only JSON file;
the gateway authoritatively resolves referenced Utility Ledger outcomes and
agent-session verification receipts before returning a contract-valid result.
Operator and Enterprise runtimes also expose the `governance` operation for
reviewed scheduling, activation, replacement, monitoring, and rollback
metadata. That paid route never runs a model, subprocess, filesystem mutation,
or Git operation; external workers retain execution ownership.

### Verified utility receipts

`contextlattice utility status` reads the public, bounded Utility Ledger. It
reports wire tokens, exact model-visible ContextLattice tokens, and observed
provider totals as separate denominators. An outcome contributes observed yield
only when its declared verification event, evidence digest, value, unit,
verifier, and pass result match exactly. Missing controls, estimated tokens,
failed verification, identity mismatch, and leakage remain visible as exclusion
evidence rather than disappearing from the denominator.

Use the primary CLI with an explicit outcome and event ID when a deterministic
test, artifact validator, external evaluator, or named human review verifies
utility. The reporting agent records the claim. The independent verifier then
appends the linked receipt under its own `--agent-id`, including after a
terminal session. The two commands may arrive in either order; the ledger
reconciles the exact event ID without turning the reporter's claim into proof:

```bash
contextlattice utility record \
  --session-id <session-id> \
  --context-pack-quality-sample-id <sample-id> \
  --outcome-id <outcome-id> \
  --utility-value 8 \
  --utility-unit acceptance_points \
  --verification-event-id <event-id> \
  --verification-evidence-digest sha256:<64-hex-evidence-digest> \
  --verification-passed true \
  --verifier-kind deterministic_test \
  --verifier-id <independent-verifier-id> \
  --latency-ms 240 --tool-calls 3 --failures 0 \
  --pretty

contextlattice utility verify \
  --agent-id <independent-verifier-id> \
  --session-id <session-id> \
  --sample-id <sample-id> \
  --outcome-id <outcome-id> \
  --utility-value 8 \
  --utility-unit acceptance_points \
  --verification-event-id <event-id> \
  --verification-evidence-digest sha256:<64-hex-evidence-digest> \
  --verification-passed true \
  --verifier-kind deterministic_test \
  --verifier-id <independent-verifier-id> \
  --pretty

contextlattice utility status --project <project> --pretty
```

Utility claims are acknowledged only after durable atomic replacement and fsync
when the ledger is enabled. Treat `utility_persistence_unavailable` as a failed
Utility Ledger receipt even though the authoritative Context Pack outcome was accepted;
retry only after storage health is restored. Any ambiguous write, fsync, or
compaction failure latches the Utility Ledger closed until runtime restart so a
retry cannot overwrite uncertain bytes. If `contextlattice utility verify`
records the authoritative verification event but cannot reconcile the durable
observation, it emits `ok:false`, preserves `event_recorded:true`, and exits
nonzero; do not replay that event blindly. Explicitly disabling the ledger
records no derived utility observations.

For causal evaluation, both arms also declare the same exact `--pair-id`,
`--experiment-id`, `--assignment-digest`, `--task-match-digest`,
`--matching-method`, `--pair-model`, `--pair-runner`, `--pair-harness`,
`--context-reconstruction-contract`, task class, project, utility unit, and
`--leakage-free true`. Treatment declares `--pair-arm treatment` and
`--matched-control-outcome-id`; controls use `--pair-arm control`. A control is
used once. Missing execution context, mixed utility units, and mismatched arms
abstain instead of manufacturing a comparison. Control and treatment must use
the same exact model-visible token count and tokenizer encoding. Negative gains remain eligible
and visible. Paid/private runtimes add `contextlattice utility analytics` and
`contextlattice utility gate`; filter policy gates with `--utility-unit` when a
ledger contains more than one unit. The gate is advisory only.

Verifier identity is locally attested, not a remote identity-provider claim:
the verification event's `agent_id` must equal `verifier_id`, and that identity
must differ from the reporting agent. Signed external evidence can be bound by
its digest without storing the artifact in ordinary memory.

## Recommended startup sequence

```bash
contextlattice doctor --pretty
contextlattice context "current task" --project contextlattice --pretty
contextlattice resume --project contextlattice --pretty
contextlattice remember "checkpoint summary" --project contextlattice --pretty
contextlattice correct "retrieval was useful" --category useful --project contextlattice --pretty
contextlattice finish "verified result" --success --project contextlattice --pretty
contextlattice utility status --project contextlattice --pretty
```

Adapter, trace, discovery, and adoption commands remain available as advanced
harness-integration and debugging surfaces; agents do not need them for the
normal task lifecycle.

## Policy Laboratory CLI

Use the CLI as the primary read surface for policy-lab status:

```bash
contextlattice_policy_lab status --pretty
```

Policy simulation and scoped cards are advisory and do not persist replay
results or activate runtime policy. Promotion recommendations are uncertainty-
aware and hold when assignment exposure, drift, or survivor-bias evidence is
incomplete. Memory retirement is propose-first and reversible; contradiction
resolution is append-only, evidence-weighted, and may abstain with an appeal
path. Storage temperature is logical retrieval-tier metadata, so moves can be
restored without deleting content or claiming physical relocation.

The public core exposes these read, simulate, recommend, propose, explicit
single-item apply, and receipt-bound restore lanes. Paid governance adds
workspace-scoped simulation history, independent project/task/intent policy
activation with bounded inheritance, canary activation and automatic rollback,
assigned contradiction review, and scheduled or batch lifecycle/temperature
runs. Configure and activate through the same CLI with `--approved`; use a
payload file for batch items and `--batch-approved` when the configured approval
threshold is crossed. Every batch is size-bounded, records intent before work,
reports partial failure honestly, and retains reversible core receipts.

Agent lifecycle state is separate from retrieval lifecycle state. Use
`idle`, `working`, `awaiting_user`, `blocked`, or `done` for the agent itself;
`retrieval_lifecycle` remains source-fetch progress such as queued, partial, or
succeeded. Each profile declares a preferred `state_authority` such as `hook`,
`self_report`, `process_probe`, or `manual`; discovery reports the evidence it
found rather than guessing.

This creates or recovers a ContextLattice-owned session, returns bounded exports,
emits a contract-valid `universal_agent_adapter_response.v1`, then lets the
agent turn session history into a bounded reference package for the next model
call. `contextlattice_agent_trace` makes that path visible as a compact terminal
tree or Markdown run card, including a list of skills that may be helpful for
the work instead of presenting skill matches as mandatory instructions. Add
`--proof` for a deterministic, read-only evidence timeline whose missing or
corrupt links remain explicit rather than inferred. For Codex hook startup,
`contextlattice_agent_start --soft --compact` still runs:
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
contextlattice_agent_runtime_doctor --pretty
```

The global runtime doctor checks Codex session storage from the installed hook
runtime. The optional `contextlattice_codex_session_store_doctor` front-door
wrapper is installed only with `scripts/install_global_agent_tools.sh --include-dev-python-tools`.
The checker resolves `~/.codex/sessions`, checks read/write/traverse access,
warns when the real path crosses `/Volumes/*` or a cloud/TCC-managed folder,
samples transcript readability, and prints the exact failing path with a
suggested fix. Warnings do not fail the aggregate agent context audit; hard
access failures do. Symlink, external-volume, and TCC topology warnings are not
permission failures. The doctor sets `permission_evidence.status=confirmed`
only for literal `EACCES`, `EPERM`, `Permission denied`, or `Operation not
permitted` evidence; when it reports `not_observed`, no permission or TCC repair
is warranted.

## Context compaction

Codex 0.130.0+ exposes `PreCompact` and `PostCompact` hook events. The installer
wires those events to ContextLattice wrappers, and
`scripts/agent/audit-compaction-hooks` verifies both the repo template and live
`~/.codex/hooks.json`. The installer also refreshes the matching
`~/.codex/config.toml` hook trust hashes so new sessions do not need repeated
manual hook review when commands are unchanged.

```bash
contextlattice_pre_compaction_write "current objective, blockers, next actions"
contextlattice_post_compaction_read  # optional manual readback; Codex hooks run a bounded read automatically
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
scoped to the interrupted session instead of broad historical notes. The
readback path is fail-open and bounded; agents should use it to recover state
after compaction, not paste large retrieved packages into every prompt.

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
contextlattice_memory_graph_repair --project my-project --pretty
contextlattice_memory_graph_repair --project my-project --write --confirm-project my-project --max-writes 500 --pretty
contextlattice_memory_graph_efficacy --refresh-cases --project my-project --graph-max-cases 3 --pretty
make memory-graph-quality-install
```

The job scores isolated docs, stale inferred edges, sparse density, and
over-connected anchors. It always runs a dry-run preflight before writes, caps
candidate and per-run write counts, scans past existing edges on later batches,
and only writes when explicit confirmation is provided. Graph efficacy cases
carry separate direct seed and graph target expectations, so direct recall and
graph lift cannot masquerade as each other. A graph hit must hydrate the target
memory into a bounded excerpt; a dangling edge cannot pass. The launchd runner defaults to dry-run mode; set
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
