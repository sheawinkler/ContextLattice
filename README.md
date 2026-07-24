# ContextLattice

<p align="center">
  <a href="https://contextlattice.io/" target="_blank" rel="noopener noreferrer">
    <img src="docs/readme/contextlattice-architecture-readme-v2-2026-04-28.png" alt="Context Lattice architecture overview poster" width="100%" />
  </a>
</p>

<p align="center">
  <strong>Open an agent. Already there.</strong>
</p>

<p align="center">
  The local-first intelligence layer that gives AI agents durable continuity, explainable retrieval, portable context, and verified learning across harnesses.
</p>

<p align="center">
  <a href="#quickstart"><img src="https://img.shields.io/badge/Interface-CLI%20First-111111?style=for-the-badge" alt="CLI first"></a>
  <a href="https://github.com/sheawinkler/ContextLattice/releases/tag/v4.0.5"><img src="https://img.shields.io/badge/Release-v4.0.5-333333?style=for-the-badge" alt="ContextLattice v4.0.5"></a>
  <a href="#quickstart"><img src="https://img.shields.io/badge/Deploy-Docker%20Compose-4b5563?style=for-the-badge" alt="Docker Compose"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache%202.0-1f2937?style=for-the-badge" alt="Apache License 2.0"></a>
</p>

[![context-lattice MCP server](https://glama.ai/mcp/servers/sheawinkler/context-lattice/badges/card.svg?v=20260324-2)](https://glama.ai/mcp/servers/sheawinkler/context-lattice)

<p align="center">
  <a href="https://contextlattice.io/">Overview</a> |
  <a href="https://contextlattice.io/architecture.html">Architecture</a> |
  <a href="https://contextlattice.io/docs/">Docs</a> |
  <a href="https://contextlattice.io/roadmap.html">Roadmap</a> |
  <a href="https://contextlattice.io/installation.html">Installation</a> |
  <a href="https://contextlattice.io/integration.html">Integrations</a> |
  <a href="https://contextlattice.io/premium.html">Premium</a> |
  <a href="https://contextlattice.io/app.html">App Surface</a> |
  <a href="https://contextlattice.io/troubleshooting.html">Troubleshooting</a> |
  <a href="https://contextlattice.io/updates.html">Updates</a>
</p>

## The Missing Layer

Models can reason. Harnesses can act. Neither one remembers the mission reliably after the chat, tool, account, or computer changes. ContextLattice closes that gap without turning every prompt into a transcript landfill.

- **Durable continuity.** Reopen the exact active mission with its checkpoint, decisions, risks, repository identity, proof, and next move already assembled.
- **Explainable retrieval.** Spend the context window on the evidence with the highest impact per token, then expose why each result was selected, opposed, deferred, or omitted.
- **Portable context.** Move signed, least-privilege continuation packets across agents, harnesses, accounts, and machines without surrendering provenance or execution control.
- **Verified skill evolution.** Convert repeated successful work into evaluated, human-approved skills; keep policy candidates shadow-only until deterministic evidence earns promotion.
- **Privacy-bounded Aggregate Signal.** Learn from explicitly opted-in, clipped statistics while raw prompts and memory stay local. Production cohort learning remains a controlled activation preview, hard-blocked until independent privacy and utility reviews pass.

## More Than Memory

ContextLattice packages the full operating layer around agent intelligence: durable writes, scoped Context Packs, deterministic synthesis, temporal claims, graph links, Skills Index discovery, behavior provenance, agent sessions, Continuity Identity, Utility Ledger economics, Context Passports, encrypted Context Mesh, Agent Fit, Policy Laboratory, Pi/Droid runner adapters, and automatic instruction hooks for supported harnesses.

The **CLI is the primary interface**. The dashboard makes behavior and proof visible. HTTP and MCP remain companion integration surfaces for applications and harnesses that need them.

`v4.0.5` is the current release baseline. The active application path is Go/Rust; Python is retained for build, development, migration, and audit tooling rather than live request handling. Public local mode is account-free. Paid artifacts add hosted distribution, workspace governance, protected operations, and advanced analytics without moving local memory into a mandatory cloud.

## Architecture Snapshot

<table>
  <tr>
    <td width="50%">
      <a href="https://contextlattice.io/architecture.html">
        <img src="docs/public_overview/assets/architecture-service-map.svg" alt="Context Lattice service map" width="100%" />
      </a>
    </td>
    <td width="50%">
      <a href="https://contextlattice.io/architecture.html">
        <img src="docs/public_overview/assets/architecture-write-flow.svg" alt="Write flow with durable outbox fanout" width="100%" />
      </a>
    </td>
  </tr>
  <tr>
    <td width="50%">
      <a href="https://contextlattice.io/architecture.html">
        <img src="docs/public_overview/assets/architecture-retrieval-flow.svg" alt="Retrieval and learning feedback flow" width="100%" />
      </a>
    </td>
    <td width="50%">
      <a href="https://contextlattice.io/architecture.html">
        <img src="docs/public_overview/assets/architecture-task-coordination.svg" alt="Task coordination and agent communication flow" width="100%" />
      </a>
    </td>
  </tr>
</table>

## Documentation

Use the repository-backed field manual as the canonical public runtime and integration guide.

- Website docs (recommended): `https://contextlattice.io/docs/`
- Canonical Markdown: `docs/wiki/`
- Deterministic builder: `scripts/build_public_docs.py`
- Retrieval receipts and trust model: [`docs/retrieval-receipts.md`](docs/retrieval-receipts.md)
- Scope: getting started, concepts, CLI, integrations, operations, troubleshooting, and releases

### Cognition Core CLI

The CLI is the prescribed agent interface. HTTP and MCP remain integration fallbacks.

```bash
# Normal task lifecycle: compact context, one reusable session, automatic outcome learning.
contextlattice doctor --pretty
contextlattice context "debug the current release regression" --project contextlattice --pretty
contextlattice remember "checkpoint summary" --project contextlattice --pretty
contextlattice resume --project contextlattice --pretty
contextlattice correct "retrieval was stale" --category stale --project contextlattice --pretty
contextlattice finish "verified result" --success --project contextlattice --pretty

# Measure useful work without mixing model-visible, wire, or provider totals.
contextlattice utility status --project contextlattice --pretty
# Record the claim, then have the named independent verifier attest the result.
contextlattice utility record --session-id <id> --context-pack-quality-sample-id <id> --outcome-id <id> --utility-value 1 --utility-unit acceptance_points --verification-event-id <id> --verification-evidence-digest sha256:<64-hex> --verification-passed true --verifier-kind deterministic_test --verifier-id <independent-verifier> --pretty
contextlattice utility verify --agent-id <independent-verifier> --session-id <id> --sample-id <id> --outcome-id <id> --utility-value 1 --utility-unit acceptance_points --verification-event-id <id> --verification-evidence-digest sha256:<64-hex> --verification-passed true --verifier-kind deterministic_test --verifier-id <independent-verifier> --pretty
# Paid analytics and gates are advisory only; they never rewrite retrieval policy.
contextlattice utility analytics --project contextlattice --pretty
contextlattice utility gate --project contextlattice --utility-unit acceptance_points --minimum-pairs 2 --pretty

# Optional delta transport for clients that retain the last trusted packet.
# A full packet is the safe fallback and becomes the next base directly.
contextlattice context "continue the release proof" --project contextlattice \
  --base-packet-file agent-packet.json --raw > packet-update.json
contextlattice packet-reconstruct --base-packet-file agent-packet.json \
  --delta-file packet-update.json --raw > next-agent-packet.json
# Pre-upgrade packets without identity metadata safely receive a new full packet.
# Packet digests detect drift but do not authenticate origin; retain the base in trusted storage.

# Advanced cognition and operator surfaces follow.
# Ask what evidence should be retrieved, from where, and when to stop.
contextlattice_retrieval_plan "debug the current release regression" --project contextlattice --pretty

# Policy Laboratory is CLI-first and advisory unless an explicit operation applies a reversible receipt.
contextlattice_policy_lab status --project contextlattice --pretty

# Inspect governed retrieval operations. Public installs discover the contract;
# paid artifacts provide stateful workspace policy and operations.
contextlattice retrieval-governance status --feature receipts --project contextlattice --pretty

# Persist a structured, time-aware assertion with explicit provenance.
contextlattice_claim_write \
  --project contextlattice \
  --subject release \
  --predicate current_version \
  --object 4.0.5 \
  --statement "The current public release is 4.0.5." \
  --pretty

# Query current, historical, superseded, or contradicted claims.
contextlattice_claim_query "current public release" --project contextlattice --include-superseded --pretty

# Return claim-level support, opposition, temporal state, uncertainty, and missing proof.
contextlattice_synthesis_pack_v2 "prove release readiness" --project contextlattice --pretty

# Resolve one durable task identity before opening another execution lane.
contextlattice_continuity_reconcile "ship continuity identity" \
  --project contextlattice --repo contextlattice --task-id frontier-t1 \
  --branch main --agent-id codex_gpt5 --pretty

# Append lifecycle evidence, inspect the objective as it existed then, and record why a decision changed.
contextlattice_objective_transition "ship continuity identity" --type progressed \
  --project contextlattice --actor codex_gpt5 \
  --idempotency-key frontier-t1-contract-proof \
  --summary "HTTP and CLI contracts verified" --pretty
contextlattice_objective_graph --project contextlattice --pretty
contextlattice_decision_change --project contextlattice --objective-id <objective-id> \
  --idempotency-key frontier-t1-semantic-abstention \
  --before "reuse every semantic match" --after "require confirmation after exact miss" \
  --confidence-before 0.45 --confidence-after 0.92 --evidence <evidence-ref> \
  --actor codex_gpt5 --rationale "Ambiguous matches must abstain." \
  --reason-code evidence_changed --pretty

# Derive a candidate only from calibration-eligible outcomes, then inspect it.
contextlattice_policy_candidate --project contextlattice --pretty
contextlattice_policy_status --pretty

# Build from repeated verified runs, test separate holdouts, then export only with approval.
contextlattice_skill_draft --payload-file workflow-runs.json --pretty
contextlattice_skill_evaluate --draft-id <draft-id> --payload-file holdouts.json --pretty
contextlattice_skill_export --draft-id <draft-id> --human-approved --approver <identity> --pretty
contextlattice_skill_retire --draft-id <draft-id> --operator <identity> --reason "temporary proof completed" --pretty

# Steer capable agents, resolve their effective profile, and request advisor-only
# runner/model selection without moving execution into the gateway.
contextlattice agent-fit steering-watch --project contextlattice --agent-id codex_gpt5 --once --raw
contextlattice agent-fit profile-resolve --project contextlattice --agent-id codex_gpt5 --raw
contextlattice agent-fit runner-select --payload-file runner-candidates.json --raw
contextlattice agent-fit context-prep-schedule --project contextlattice --payload-file prep-request.json --raw

# Continuity Zero: open the one unambiguous active mission already assembled.
# The optional artifact is written owner-only; no local path is sent to the gateway.
contextlattice_continuity_zero --project contextlattice --agent codex \
  --output continuity-zero.json --pretty

# Carry signed evidence across machines without surrendering trust or transport.
contextlattice_passport_export "prepare the release handoff" --project contextlattice --output passport.json --pretty
contextlattice_passport_verify --file passport.json --pretty
contextlattice_mesh_identity --pretty

# Portable Continuation: grant narrowly, seal locally, move through your chosen
# channel, then decrypt and reconcile without importing or executing anything.
contextlattice_agent_tools portable-continuation grant-create \
  --payload-file grant-request.json --output grant.json --pretty
contextlattice_agent_tools portable-continuation manifest-create \
  --payload-file continuation-request.json --output continuation-envelope.json --pretty
contextlattice_agent_tools portable-continuation manifest-reconcile \
  --payload-file reconciliation-request.json --envelope-file continuation-envelope.json --pretty
contextlattice_agent_tools portable-continuation status --pretty
```

The planner remains `shadow_only`: learned candidates are advice, not a hidden configuration rewrite. Skill exports remain inactive files until reviewed and installed through the normal Skills Index workflow. Passport replay never executes imported text, and Mesh never owns transport. See [`docs/continuity-identity.md`](docs/continuity-identity.md), [`docs/cognition-proof-core.md`](docs/cognition-proof-core.md), [`docs/outcome-policy-skill-foundry.md`](docs/outcome-policy-skill-foundry.md), and [`docs/context-passport-mesh.md`](docs/context-passport-mesh.md) for contracts, safety boundaries, and HTTP fallbacks.

## Quickstart

### Agent Quickstart: Install + Integrate

If you are an AI agent reading this README because a user asked you to install or integrate ContextLattice, follow this section directly. Do not wait for the user to copy another prompt back to you.

Human users can also paste the block below into Codex, Claude Code, Hermes, OpenCode, OMP, Mercury, or another terminal-capable agent.

```text
You are integrating ContextLattice.

Repository:
https://github.com/sheawinkler/ContextLattice.git

Goals:
- Install and launch ContextLattice locally.
- Verify the runtime is healthy.
- Integrate the target repo with ContextLattice-supported agent profiles.
- Do not install third-party agent harness binaries unless the user explicitly asks.

Important:
- `contextlattice_adopt integrate` writes managed instruction blocks for supported profiles.
- Installer flows may add managed instruction hooks for detected OMP and Mercury user-instruction files; they do not install OMP, Mercury, Codex, Claude Code, Hermes, Hermes Ultra, Pi, Droid, or other agent binaries.
- Optional Pi/Droid runner execution requires their CLIs (`brew install pi-coding-agent`, `brew install --cask droid`).
- Preserve existing user text in repo instruction files.
- If you are already inside a ContextLattice checkout, do not clone a duplicate repo.

Install ContextLattice:

git clone https://github.com/sheawinkler/ContextLattice.git
cd ContextLattice
cp .env.example .env
gmake quickstart

Verify ContextLattice:

curl -fsS http://127.0.0.1:8075/health | jq
contextlattice_adopt status --pretty

Integrate the target repo:

cd /path/to/target/repo
contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --pretty
contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --check --pretty
contextlattice_doctor --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --skip-provider-smoke --pretty

If any step fails:
- Report the exact failing command and path.
- Fix only the local setup issue needed for that failure.
- Rerun the check.
- If ContextLattice is unreachable, continue in degraded-memory mode and say so explicitly.
```

For the full reusable agent contract, see `docs/public_overview/templates/agents/universal.md`.

### 1) Clone and configure

```bash
git clone git@github.com:sheawinkler/ContextLattice.git
cd ContextLattice
cp .env.example .env
```

### 2) Launch (recommended)

```bash
gmake quickstart
```

`gmake quickstart` prompts for runtime profile and then launches the selected stack.
It is the canonical first-install path; signed installers are bootstrap alternatives, not a competing control surface.

### 3) Verify

```bash
curl -fsS http://127.0.0.1:8075/health | jq
scripts/agent/agent-runtime-proof-pack --pretty
scripts/agent/agent-adoption-proof-matrix --skip-provider-smoke --progress --pretty
```

Expected:
- `/health` returns `{"ok": true, ...}`
- `agent-runtime-proof-pack` completes bootstrap, scoped recall, checkpoint, handoff, completion, status, prompt context package, and runtime telemetry phases.
- `agent-adoption-proof-matrix` verifies configured agent profiles and reports the skills, context, session, graph, and handoff evidence shaping each run, with trace commands for run-card export.



## Model Runtime

Task inference defaults to `ORCH_INFER_PROVIDER=auto`. `gateway-go` detects the host profile and probes local backends before selecting a route.

- Apple Silicon default priority: `mlx,vllm-metal,ane_sidecar,llama-cpp,ollama`.
- CUDA/ROCm default priority: `sglang,vllm,openai-compatible,llama-cpp,lmstudio,ollama`.
- Generic CPU default priority: `openai-compatible,llama-cpp,lmstudio,ollama`.
- Supported provider ids include `sglang`, `vllm`, `vllm-metal`, `mlx`, `mtplx` (alias for MLX), `openai-compatible`, `lmstudio`, `llama-cpp`, `tgi`, `tensorrt-llm`, `ane_sidecar`, and `ollama`.
- `/v1/inference/runtime-policy` returns live provider health plus resource-aware model guidance. If host memory/VRAM is not identifiable, it falls back to generic local advice: start with Q4/IQ4 7B-9B models, benchmark, then scale up.
- The current opt-in local model shortlist lives in `docs/runtime/local-model-options.md`; it includes small/medium MLX, GGUF, and HF/safetensors candidates plus frontier-provider connection guidance. GGUF models use an external llama.cpp-compatible connector; ContextLattice does not start or bundle llama.cpp in Lite.
- Large Qwen3.6 Dream Mode models are opt-in only; ContextLattice does not bundle or pull them by default. The default GGUF recommendation is `mudler/Qwen3.6-35B-A3B-Claude-4.7-Opus-Reasoning-Distilled-APEX-MTP-GGUF` for llama.cpp-compatible advanced users. Abliterated variants are private-eval only behind `CONTEXTLATTICE_DREAM_ALLOW_PRIVATE_EVAL_MODELS=true` (`GO_DREAM_ALLOW_UNCENSORED_MODELS=true` remains a legacy alias).
- Inference runtimes must emit final assistant content through their API. Reasoning-only responses fail with repair instructions instead of being accepted. For MLX Qwen thinking templates, use `scripts/inference_mlx_server.sh --model /path/to/mlx/model --template-profile qwen-final-content`, then verify with `scripts/inference_template_conformance.sh --provider mlx --model /path/to/mlx/model`.
- Dream Mode reflects on LLM-generated hypotheses by default and performs one bounded deepening pass when the best output misses the sigma target (`GO_DREAM_REFLECT_ENABLED=true`, `GO_DREAM_DEEPEN_ON_WEAK_OUTPUT=true`, `GO_DREAM_REFLECTION_MIN_SCORE=0.74`). If structured LLM synthesis is unavailable, Dream Mode returns `dream_unavailable`; non-LLM evidence packaging belongs to context-pack or review.
- Ollama remains a compatibility fallback, not the preferred always-on embedding path.
- Local helpers enforce one active LLM backend by default (`CONTEXTLATTICE_SINGLE_ACTIVE_INFER_BACKEND=true`).

Inspect live routing and benchmark configured backends:

```bash
scripts/inference_runtime_policy.sh
scripts/benchmark_inference_backends.sh
scripts/inference_template_conformance.sh --provider mlx --model /path/to/mlx/model
```

Embedding defaults to the Rust `fastembed-rs` sidecar. Ollama stays available as an explicit compatibility fallback, not the preferred embedding path.

Useful model runtime knobs:

```bash
ORCH_INFER_PROVIDER=auto
ORCH_INFER_PROVIDER_PRIORITY=mlx,vllm-metal,ane_sidecar,sglang,vllm,openai-compatible,llama-cpp,ollama
ORCH_INFER_AUTO_PROBE_ENABLED=true
SGLANG_BASE_URL=http://127.0.0.1:30000
VLLM_BASE_URL=http://127.0.0.1:8000
VLLM_METAL_BASE_URL=http://127.0.0.1:8000
MLX_API_BASE=http://127.0.0.1:18087/v1
LLAMA_CPP_BASE_URL=http://127.0.0.1:8080
```

## Agent CLI

Installer and quickstart paths install agent helpers under `$HOME/.contextlattice/bin`.

```bash
contextlattice_agent_adapter profiles
contextlattice_adopt status --pretty
contextlattice_doctor --agents codex --skip-provider-smoke --pretty
contextlattice_agent_start --soft --compact
contextlattice_agent_trace --session-id <session-id> --tree
contextlattice_pack "what should the next agent know?" --project my-project --pretty
contextlattice_search -h
contextlattice_write -h
contextlattice_checkpoint -h
contextlattice_skills_index search "browser automation" --pretty
contextlattice_passport_export "portable task context" --project my-project --output passport.json --pretty
contextlattice_mesh_status --pretty
```

- `contextlattice_agent_adapter` is the first-class lifecycle helper for bootstrap, context-pack, checkpoint, handoff, state, event, and completion flows.
- `contextlattice_agent_adapter state --state working|awaiting_user|blocked|done --session-id <id> --pretty` reports semantic agent lifecycle state with authority, source, TTL, native session id, task id, repo, worktree, branch, cwd, and user/blocker fields.
- `contextlattice_agent_discover --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --repo . --pretty` reports best-effort local process/profile/hook/repo-instruction evidence with explanations. Discovery is diagnostic evidence; explicit hook or adapter state remains authoritative.
- `contextlattice_adopt` is the zero-friction front door for local readiness, install guidance, profiles, and lifecycle proof; `contextlattice_doctor` combines readiness, proof, and trace evidence in one bounded report.
- `contextlattice_agent_start` runs the lightweight startup guard for agents.
- `contextlattice_agent_trace` renders the bounded run-shaping trail as a terminal tree, JSON, or Markdown run card.
- `contextlattice_pack` compiles a bounded prompt-ready packet with ranked evidence, files to inspect, risks, checks, source coverage, and a `reference_prompt`.
- `contextlattice_checkpoint` writes a checkpoint and verifies readback.
- `contextlattice_skills_index` discovers capabilities without loading every skill into startup context.
- `contextlattice_passport_*` signs, verifies, diffs, replays, and imports bounded context without executing imported instructions.
- `contextlattice_mesh_*` manages public recipient identity, grants, encrypted file/JSON envelopes, dry-run reconciliation, and explicit apply; transport remains caller-owned.
- `contextlattice_source_backfill` is an optional development helper, installed with `scripts/install_global_agent_tools.sh --include-dev-python-tools`, for bounded data imports.
- Hook pack details: `docs/agent-hooks.md`.

## Agent Runtime Sessions

ContextLattice tracks live agent work as first-class sessions, independent of the runner or model provider.

- Start/list/read sessions through `GET|POST /v1/agents/sessions` and `GET /v1/agents/sessions/{session_id}`.
- Emit normalized events through `POST /v1/agents/sessions/event` or `POST /v1/agents/sessions/{session_id}/events`.
- Inspect a bounded run trace through `GET /v1/agents/sessions/{session_id}/trace`; the trace reports context, skills that may be helpful, source coverage, graph touches, handoffs, checkpoints, and timeline events without raw provider payloads.
- Read live runtime telemetry from `GET /telemetry/agents/runtime`.
- Compile task context through `POST /memory/context-pack`, `POST /tools/context_pack`, or global `contextlattice_pack`; responses include `context_compiler`, ranked evidence, deterministic `agent_guidance` for themes/risk markers/candidate attention links, prompt sections, and a bounded `reference_prompt`.
- Ask for Synthesis Pack v1 through global `contextlattice_synthesis_pack "<task>" --project <project> --pretty`, `POST /memory/synthesis-pack`, or `POST /tools/synthesis_pack` when the next agent needs grouped high-signal findings, topic gravity, graph/cross-project bridges, must-not-forget constraints, recommended next actions, open questions, and semantic tags over the same bounded evidence.
- Watch long-running recall through `scripts/agent/contextlattice-session watch --session-id <id> --continuation-token <token>`; continuation responses include `retrieval_progress.v1`, dashboard status links, and agent-visible steering when async work is ready.
- Preflight, context-pack, and Dream Mode return `objective_runtime_state.v1` with `objective_state`, `action_executed`, `evidence`, `objective_delta`, `risk_or_blocker`, and `next_action`.
- Use `scripts/agent/contextlattice-agent-adapter` or global `contextlattice_agent_adapter` as the first-class product path for agent bootstrap, context-pack, checkpoint, handoff, state, event, and completion flows.
- Use `contextlattice_agent_adapter state --state working|awaiting_user|blocked|done --session-id <id> --pretty` to report semantic agent lifecycle state with authority, source, TTL, native session id, task id, repo, worktree, branch, cwd, and user/blocker fields.
- Use global `contextlattice_agent_discover --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --repo . --pretty` for best-effort local process/profile/hook/repo-instruction discovery. Discovery explains why ContextLattice believes an agent is idle, working, waiting, blocked, or integrated; it does not replace explicit hook or adapter state.
- Use `scripts/agent/contextlattice-adopt` or global `contextlattice_adopt` before handing ContextLattice to a new agent/account; `doctor` combines gateway health, helper install state, shell PATH, storage posture, session store, profile coverage, best-effort discovery, runtime-doctor checks, lifecycle proof, and run trace evidence into one bounded report.
- Run `contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid --pretty` inside a target repo to write managed `AGENTS.md`, `CLAUDE.md`, `HERMES.md`, `MERCURY.md`, `PI.md`, and `DROID.md` instruction blocks without overwriting user text.
- Run `contextlattice_doctor --agents codex --skip-provider-smoke --pretty` for the fastest new-agent adoption proof.
- The same doctor works for other agent profiles: `contextlattice_doctor --agents claude-code --skip-provider-smoke --pretty`, `contextlattice_doctor --agents opencode --skip-provider-smoke --pretty`, or `contextlattice_doctor --agents codex,claude-code,opencode --skip-provider-smoke --pretty`.
- Run `scripts/agent/agent-runtime-proof-pack --pretty` or global `contextlattice_agent_runtime_proof --pretty` for a one-command live proof that bootstrap, scoped recall, checkpoint, handoff, completion, status, and runtime telemetry are wired end to end.
- Use `scripts/agent/contextlattice-session` for CLI start/event/complete/fail/status/runtime/trace flows.
- Use `scripts/agent/agent-run-trace --session-id <id> --tree` or global `contextlattice_agent_trace --session-id <id> --tree` to see the terminal trace, then `--markdown` to export the run card.
- Use global `contextlattice_runner_quality --pretty` to inspect bounded runner-quality telemetry for adapter success/block/failure rates, context-pack quality linkage, exact prompt-token savings, modeled inference-avoidance signals, and advisor-only runner recommendations. Repo-local `scripts/agent/runner-quality --pretty` remains available for development fallback.
- Use `scripts/agent/contextlattice-session sweep-stale-audits --all-projects --pretty` for dry-run-first cleanup of stale objective-runtime audit/preflight sessions; add `--confirm` only after reviewing matches.
- `scripts/agent/contextlattice-pack`, `scripts/agent/contextlattice-dream`, `scripts/agent/writeback`, and compaction hooks auto-start or recover a session when `CONTEXTLATTICE_SESSION_ID` is absent.
- Pass `--session-id` or `CONTEXTLATTICE_SESSION_ID` to force a specific session. Set `CONTEXTLATTICE_AUTO_SESSION_DISABLED=1` to disable automatic session creation.

Canonical event families include `session.started`, `agent.state.working`, `agent.state.awaiting_user`, `agent.state.blocked`, `agent.state.done`, `context_pack.completed`, `retrieval.continuation.progress`, `retrieval.continuation.ready`, `retrieval.continuation.degraded`, `dream.completed`, `graph.neighbors_returned`, `graph.edge_touched`, `decision.made`, `test.ran`, `handoff.created`, `writeback.completed`, and `session.completed`.

Agent lifecycle and retrieval lifecycle are intentionally separate. `agent_lifecycle.state` describes the actor (`idle`, `working`, `awaiting_user`, `blocked`, `done`); `retrieval_lifecycle.status` describes source-fetch progress. This prevents async retrieval warming from being misread as a blocked or degraded agent.

Runner quality is intentionally separate from both lifecycle surfaces. It is advisor-only telemetry for operator selection, not automatic dispatch. See `docs/runtime/runner-quality-loop.md` for the compact adapter measurement loop and its interpretation limits.

## Download Installers

- macOS DMG: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-macOS-universal.dmg`
- macOS signing/notarization operator notes: `docs/releases/macos-signing-notarization.md`
- Homebrew cask: `brew tap sheawinkler/contextlattice && brew install --cask contextlattice`
- Windows MSI: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-windows-x64.msi`
- Linux bundle: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-linux-bootstrap.tar.gz`

## Resource Profiles

| Profile | CPU | RAM | Storage |
| --- | --- | --- | --- |
| Lite core | `2-4` vCPU | `8-12 GB` | `25-80 GB` |
| Lite advanced | `4-6` vCPU | `12-16 GB` | `80-140 GB` |
| Full | `6-8` vCPU | `12-20 GB` | `100-180 GB` |

Optional constrained-disk guard: set `QDRANT_HOT_STORAGE_MAX_BYTES` to make launch/storage verification fail before the Qdrant hot lane exceeds your chosen byte ceiling. This is a guardrail, not a filesystem quota.

Optional external model storage: set `OLLAMA_DATA` to an absolute path to keep local Ollama model blobs off a constrained internal disk. When unset, the existing home-directory model store remains the backward-compatible default; startup storage verification rejects mount drift.

## Memory Graph

- `GET|POST /v1/memory/edges` persists explicit typed relationships.
- `POST /v1/memory/edges/backfill` audits or applies deterministic retroactive edges and opt-in same-project `inferred_related` scoring. It is dry-run by default.
- `POST /v1/memory/neighbors` returns explicit/inferred edge neighbors merged with semantic/topic neighbors.

```bash
contextlattice_memory_graph_repair --project my-project --pretty
contextlattice_memory_graph_repair --project my-project --write --confirm-project my-project --max-writes 500 --pretty
contextlattice_memory_graph_efficacy --refresh-cases --project my-project --graph-max-cases 3 --pretty

# Repo-local advanced fallback; inferred scoring is opt-in.
./scripts/agent/memory-edge-backfill --project my-project --max-writes 500
./scripts/agent/memory-edge-backfill --project my-project --include-inferred --min-confidence 0.90
./scripts/agent/memory-edge-inferred-retrofill --project my-project --corpus disk --profile exploratory
```

## Source Backfill

Bring existing data into ContextLattice without changing the ingest boundary.
Backfill is dry-run by default, writes go through `/memory/write`, and writes
require `--write --confirm-write <project>`.

```bash
./scripts/agent/source-backfill-memory --source jsonl --path exports/tasks.jsonl --project my-project --pretty
./scripts/agent/source-backfill-memory --source sqlite --path app.db --table notes --project my-project --pretty
./scripts/agent/source-backfill-memory --source parquet --path warehouse/events.parquet --project my-project --pretty
./scripts/agent/source-backfill-memory --source postgres --dsn "$DATABASE_URL" --query "select id,title,body from notes limit 100" --project my-project --pretty
./scripts/agent/source-backfill-memory --source jsonl --path exports/tasks.jsonl --project my-project --write --confirm-write my-project --apply-edges --pretty
```

Supported adapters: files/directories, JSONL, JSON, CSV, SQLite, DuckDB, Parquet
via DuckDB, and Postgres via optional `psycopg`. Import caps cover records, row
bytes, document bytes, total bytes, and structured-list items. Secret-like
fields are redacted by default, and graph edge repair is optional and bounded.

## Skills Index And Quarantine Discovery

ContextLattice exposes active skills as a native Go Skills Index so agents can discover relevant capabilities without loading every `SKILL.md` into prompt context. In local installs, the active index mounts `${HOME}/.codex/skills` read-only by default. Quarantined/vendor skill discovery remains a separate read-only lane and does **not** auto-load quarantined skills.

- Active index endpoint: `GET|POST /v1/skills/index/search`
- Active index tool: `GET|POST /tools/skills_index_search`
- Active index status/reindex endpoint: `POST /v1/skills/index/reindex` (live native scan; no prompt loading)
- Search endpoint: `GET|POST /v1/skills/quarantine/search`
- Tool alias: `GET|POST /tools/skills_quarantine_search`
- Reindex endpoint: `POST /v1/skills/quarantine/reindex` (off by default; enable explicitly)

Runtime knobs:

```bash
ORCH_SKILLS_QUARANTINE_ENABLED=true
ORCH_SKILLS_QUARANTINE_HOST_BIN_DIR=${HOME}/.local/bin
ORCH_SKILLS_INDEX_HOST_ACTIVE_ROOT_DIR=${HOME}/.codex/skills
ORCH_SKILLS_INDEX_HOST_SYSTEM_ROOT_DIR=${HOME}/.codex/skills/.system
ORCH_SKILLS_INDEX_ROOTS=/opt/contextlattice/skills_active:/opt/contextlattice/skills_system
ORCH_SKILLS_QUARANTINE_HOST_ROOT_DIR=${HOME}/.codex/skills_quarantine
ORCH_SKILLS_QUARANTINE_SEARCH_CMD=/opt/contextlattice/skills/bin/codex-skills-quarantine-search
ORCH_SKILLS_QUARANTINE_REINDEX_CMD=/opt/contextlattice/skills/bin/codex-skills-quarantine-reindex
ORCH_SKILLS_QUARANTINE_TIMEOUT_SECS=8
ORCH_SKILLS_QUARANTINE_DEFAULT_LIMIT=20
ORCH_SKILLS_QUARANTINE_MAX_LIMIT=100
ORCH_SKILLS_QUARANTINE_REINDEX_ENABLED=false
CODEX_SKILLS_QUARANTINE_ROOT=/opt/contextlattice/skills_quarantine
CODEX_SKILLS_QUARANTINE_INDEX_DIR=/opt/contextlattice/skills_quarantine/index
CODEX_SKILLS_QUARANTINE_INDEX=/opt/contextlattice/skills_quarantine/index/skills_index.jsonl
```

## Security and Privacy

- Local-first by default.
- API-key protected operational routes.
- Secret-like content redaction controls.
- Premium billing/provider route maps are intentionally kept out of public docs.

## Docs Index

- Overview: `https://contextlattice.io/`
- Architecture: `https://contextlattice.io/architecture.html`
- Local AI workspace comparison: `https://contextlattice.io/local-ai-workspaces.html`
- Scaling memory: `https://contextlattice.io/scaling-memory.html`
- Wiki: `https://contextlattice.io/wiki.html`
- Installation: `https://contextlattice.io/installation.html`
- Integrations: `https://contextlattice.io/integration.html`
- Troubleshooting: `https://contextlattice.io/troubleshooting.html`
- Updates: `https://contextlattice.io/updates.html`
- Plans and distribution boundaries: `docs/public_overview/premium.html`
- Retrieval receipts and trust model: [`docs/retrieval-receipts.md`](docs/retrieval-receipts.md)
- Release notes, newest first; older entries are historical:
  - `docs/releases/v4.0.5.md` (large-store retrieval stability, dependency hardening, responsive dashboard proof, and honest macOS distribution)
  - `docs/releases/v4.0.4.md` (fail-closed host supervision, bounded recovery, truthful unload, and safe upgrade migration)
  - `docs/releases/v4.0.3.md` (production dashboard runtime, mode-aware probes, responsive graph paths, and paid replay integrity)
  - `docs/releases/v4.0.2.md` (one product truth across CLI, dashboard, docs, installers, release gates, and lane licensing)
  - `docs/releases/v4.0.1.md` (public installer boundary repair with unchanged Aggregate Signal runtime semantics)
  - `docs/releases/v4.0.0.md` (privacy-bounded Aggregate Signal, explicit consent, cohort suppression, and proof-held paid governance)
  - `docs/releases/v3.26.0.md` (zero-entry continuity, fail-closed objective selection, and governed external-adapter intents)
  - `docs/releases/v3.25.0.md` (evidence-qualified skill evolution, atomic Foundry handoff, and exact-receipt governance)
  - `docs/releases/v3.24.0.md` (signed collaborative grants, provenance-preserving imports, and encrypted replay-safe continuation)
  - `docs/releases/v3.23.0.md` (Agent Fit steering, advisor-only selection, adaptive profiles, and explicit context preparation)
  - `docs/releases/v3.22.0.md` (Policy Laboratory simulation, scoped learning, reversible lifecycle, contradiction, and storage governance)
  - `docs/releases/v3.21.0.md` (retrieval receipts, trust isolation, ablation, causal proof, and governed paid operations)
  - `docs/releases/v3.20.3.md` (complete evaluation gate and governed efficiency-policy entitlement)
  - `docs/releases/v3.20.2.md` (source-identical paid release tests and exact artifact provenance)
  - `docs/releases/v3.20.1.md` (current-release provenance binding and historical-proof separation)
  - `docs/releases/v3.20.0.md` (verified Utility Ledger, exact context economics, and advisory paid analytics)
  - `docs/releases/v3.19.1.md` (panic-free terminal session commands, canonical lifecycle events, and legacy-client healing)
  - `docs/releases/v3.19.0.md` (digest-verified Agent Packet deltas, exact reconstruction, and one gap-aware proof timeline)
  - `docs/releases/v3.18.0.md` (durable continuity identity, holdout-locked semantic reconciliation, shared objective graphs, and witnessed decision provenance)
  - `docs/releases/v3.17.5.md` (latest-only configured state, bounded owner-only migration, and fail-closed semantic readers)
  - `docs/releases/v3.17.4.md` (regression-locked public artifact boundary and complete installer publication)
  - `docs/releases/v3.17.3.md` (owner-only local stores, approval-before-work, evidence-qualified advice, and exact runtime truth)
  - `docs/releases/v3.17.2.md` (operator-chosen Ollama storage, verified mount truth, and sourceable environment profiles)
  - `docs/releases/v3.17.0.md` (canonical commercial truth, self-contained installers, strict public boundary, and tiny-pack identity)
  - `docs/releases/v3.16.1.md` (contract-valid compact lifecycle receipts for the primary CLI)
  - `docs/releases/v3.16.0.md` (compact Agent Packet, session truth, monotonic async, outcome learning, unified CLI, and proof workbench)
  - `docs/releases/v3.15.1.md` (Node 24 LTS convergence across local tooling, containers, packages, and release actions)
  - `docs/releases/v3.15.0.md` (bounded graph repair, explicit graph efficacy, hydrated neighbors, and durable Foundry retirement)
  - `docs/releases/v3.14.0.md` (signed Context Passport and encrypted Context Mesh)
  - `docs/releases/v3.13.0.md` (outcome-trained canary policy and Skill Foundry)
  - `docs/releases/v3.12.0.md` (Temporal Claim Graph, adaptive retrieval planning, and Proof-Carrying Synthesis v2)
  - `docs/releases/v3.11.2.md` (evidence-preserving sparse Context Packs and grounded deterministic synthesis)
  - `docs/releases/v3.11.1.md` (upgrade-safe runner discovery across long-lived agent environments)
  - `docs/releases/v3.11.0.md` (Synthesis Packs, async warming, outcome calibration, memory activation evidence, and public-core parity)
  - `docs/releases/v3.10.2.md` (Go-native feedback submit, idempotency, preference projection, and strict ownership coverage)
  - `docs/releases/v3.10.1.md` (detected OMP/Mercury instruction hooks and default adoption coverage)
  - `docs/releases/v3.10.0.md` (optional Pi/Droid runner adapters, runner-quality advisor, and CLI-first public surface)
  - `docs/releases/v3.9.1.md` (dashboard contrast, settings clarity, public-site alignment, and current-version guidance)
  - `docs/releases/v3.9.0.md` (agent-operable context-pack outcome telemetry and observed provider usage)
  - `docs/releases/v3.8.0.md` (MongoDB driver v2 migration and Context Pack Quality Ledger)
  - `docs/releases/v3.7.1.md` (MongoDB driver security patch for the v3.7 train)
  - `docs/releases/v3.7.0.md` (tokenizer-exact prompt economics, bounded token-impact ledger, and release-note hygiene)
  - `docs/releases/v3.6.2.md`
  - `docs/releases/v3.5.0.md`
  - `docs/releases/v3.4.25.md`
  - `docs/releases/v3.4.14.md`
  - `docs/releases/v3.4.13.md`
  - `docs/releases/v3.4.12.md`
  - `docs/releases/v3.4.11.md`
  - `docs/releases/v3.4.10.md`
  - `docs/releases/v3.4.5.md`
  - `docs/releases/v3.4.2.md`
  - `docs/releases/v3.4.1.md`
- Local model options: `docs/runtime/local-model-options.md`


## License

Apache License 2.0 (`LICENSE`).
