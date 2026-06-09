# ContextLattice

<p align="center">
  <a href="https://contextlattice.io/" target="_blank" rel="noopener noreferrer">
    <img src="docs/readme/contextlattice-architecture-readme-v2-2026-04-28.png" alt="ContextLattice architecture overview" width="100%" />
  </a>
</p>

<p align="center">
  Private-by-default memory and context orchestration for AI agents.
</p>

<p align="center">
  <a href="https://modelcontextprotocol.io/"><img src="https://img.shields.io/badge/MCP-HTTP%20Gateway-6b7280?style=for-the-badge" alt="MCP HTTP Gateway"></a>
  <a href="#quickstart"><img src="https://img.shields.io/badge/Deploy-Docker%20Compose-4b5563?style=for-the-badge" alt="Docker Compose"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-BSL%201.1-1f2937?style=for-the-badge" alt="BSL 1.1"></a>
</p>

[![context-lattice MCP server](https://glama.ai/mcp/servers/sheawinkler/context-lattice/badges/card.svg?v=20260324-2)](https://glama.ai/mcp/servers/sheawinkler/context-lattice)

## What ContextLattice Does

ContextLattice provides a single memory contract for agentic systems:

- Unified write/read contract for memory and context.
- Durable fanout across retrieval/storage lanes.
- Staged retrieval (fast now, deep continuation when needed).
- Go/Rust runtime ownership for the active application path.
- Legacy Python runtime archived under `archive/services/orchestrator_legacy_python` for tooling/test compatibility only.
- Local-first deployment with optional hosted surfaces.

## Current Public Baseline

`v3.4.2` is the public agent runtime contract baseline: universal adapter lifecycle, native agent sessions, objective runtime state, scoped recall, checkpoints, handoffs, completion flow, runtime telemetry, one-command runtime proof, storage-governance hardening, and local session-store diagnostics behind one local contract.

`v4` remains the private tuning lane for experiments that still need benchmark, recall, and soak gates before public promotion.

## Public Runtime Stack (v3.4)

- Ingress: `gateway-go`.
- Core memory + retrieval lanes: Go + Rust services.
- Degradation policy: fail-open retrieval with continuation lifecycle.
- Tooling compatibility: MCP + HTTP clients.
- Single-container lite builds (`Dockerfile.hf-lite`) also run `gateway-go` (no Python runtime dependency).
- Public single-container lite vector default: `topic_rollups` only.
- Public local lite core default: `topic_rollups + qdrant`; pgvector and memory-bank spike adapters are not started by default.
- Public local lite advanced: opt-in adapter lab via `gmake mem-up-lite-advanced`.
- Full/operator stacks: Qdrant remains the primary vector-native lane; pgvector stays supported for SQL-co-located vector workloads.

## Quickstart

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

### 3) Verify

```bash
curl -fsS http://127.0.0.1:8075/health | jq
scripts/agent/agent-runtime-proof-pack --pretty
scripts/agent/agent-adoption-proof-matrix --skip-provider-smoke --pretty
```

Expected:
- `/health` returns `{"ok": true, ...}`
- `agent-runtime-proof-pack` completes bootstrap, scoped recall, checkpoint, handoff, completion, status, prompt context package, and runtime telemetry phases.
- `agent-adoption-proof-matrix` verifies configured agent profiles and reports the skills, context, session, graph, and handoff evidence shaping each run.

## Model Runtime

Task inference defaults to `ORCH_INFER_PROVIDER=auto`. `gateway-go` detects the host profile and probes local backends before selecting a route.

- Apple Silicon default priority: `mlx,vllm-metal,ane_sidecar,llama-cpp,ollama`.
- CUDA/ROCm default priority: `sglang,vllm,openai-compatible,llama-cpp,lmstudio,ollama`.
- Generic CPU default priority: `openai-compatible,llama-cpp,lmstudio,ollama`.
- Supported provider ids include `sglang`, `vllm`, `vllm-metal`, `mlx`, `mtplx` (alias for MLX), `openai-compatible`, `lmstudio`, `llama-cpp`, `tgi`, `tensorrt-llm`, `ane_sidecar`, and `ollama`.
- `/v1/inference/runtime-policy` returns live provider health plus resource-aware model guidance. If host memory/VRAM is not identifiable, it falls back to generic local advice: start with Q4/IQ4 7B-9B models, benchmark, then scale up.
- Large Qwen3.6 Dream Mode models are opt-in only; ContextLattice does not bundle or pull them by default. The default GGUF recommendation is `mudler/Qwen3.6-35B-A3B-Claude-4.7-Opus-Reasoning-Distilled-APEX-MTP-GGUF` for llama.cpp-compatible advanced users. Abliterated variants are private-eval only behind `CONTEXTLATTICE_DREAM_ALLOW_PRIVATE_EVAL_MODELS=true` (`GO_DREAM_ALLOW_UNCENSORED_MODELS=true` remains a legacy alias).
- Inference runtimes must emit final assistant content through their API. Reasoning-only responses fail with repair instructions instead of being accepted. For MLX Qwen thinking templates, use `scripts/inference_mlx_server.sh --model /path/to/mlx/model --template-profile qwen-final-content`, then verify with `scripts/inference_template_conformance.sh --provider mlx --model /path/to/mlx/model`.
- Dream Mode reflects on generated hypotheses by default and performs one bounded deepening pass when the best output misses the sigma target (`GO_DREAM_REFLECT_ENABLED=true`, `GO_DREAM_DEEPEN_ON_WEAK_OUTPUT=true`, `GO_DREAM_REFLECTION_MIN_SCORE=0.74`).
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

Installer and quickstart paths install agent helpers under `~/.contextlattice/bin`.

```bash
contextlattice_agent_adapter profiles
contextlattice_agent_start --soft --compact
contextlattice_pack "what should the next agent know?" --project my-project --pretty
contextlattice_search -h
contextlattice_write -h
contextlattice_checkpoint -h
contextlattice_source_backfill --source jsonl --path data.jsonl --project my-project --pretty
```

- `contextlattice_agent_adapter` is the first-class lifecycle helper for bootstrap, context-pack, checkpoint, handoff, event, and completion flows.
- `contextlattice_agent_start` runs the lightweight startup guard for agents.
- `contextlattice_pack` compiles a bounded prompt-ready packet with ranked evidence, files to inspect, risks, checks, source coverage, and a `reference_prompt`.
- `contextlattice_checkpoint` writes a checkpoint and verifies readback.
- `contextlattice_source_backfill` imports bounded files, JSONL, JSON, CSV, SQLite, DuckDB/Parquet, or Postgres data through the same memory write contract.
- Hook pack details: `docs/agent-hooks.md`.

## Agent Runtime Sessions

ContextLattice tracks live agent work as first-class sessions, independent of the runner or model provider.

- Start/list/read sessions through `GET|POST /v1/agents/sessions` and `GET /v1/agents/sessions/{session_id}`.
- Emit normalized events through `POST /v1/agents/sessions/event` or `POST /v1/agents/sessions/{session_id}/events`.
- Read live runtime telemetry from `GET /telemetry/agents/runtime`.
- Compile task context through `POST /memory/context-pack`, `POST /tools/context_pack`, or global `contextlattice_pack`; responses include `context_compiler`, ranked evidence, prompt sections, and a bounded `reference_prompt`.
- Preflight, context-pack, and Dream Mode return `objective_runtime_state.v1` with `objective_state`, `action_executed`, `evidence`, `objective_delta`, `risk_or_blocker`, and `next_action`.
- Use `scripts/agent/contextlattice-agent-adapter` or global `contextlattice_agent_adapter` as the first-class product path for agent bootstrap, context-pack, checkpoint, handoff, event, and completion flows.
- Run `scripts/agent/agent-runtime-proof-pack --pretty` or global `contextlattice_agent_runtime_proof --pretty` for a one-command live proof that bootstrap, scoped recall, checkpoint, handoff, completion, status, and runtime telemetry are wired end to end.
- Use `scripts/agent/contextlattice-session` for CLI start/event/complete/fail/status/runtime flows.
- Use `scripts/agent/contextlattice-session sweep-stale-audits --all-projects --pretty` for dry-run-first cleanup of stale objective-runtime audit/preflight sessions; add `--confirm` only after reviewing matches.
- `scripts/agent/contextlattice-pack`, `scripts/agent/contextlattice-dream`, `scripts/agent/writeback`, and compaction hooks auto-start or recover a session when `CONTEXTLATTICE_SESSION_ID` is absent.
- Pass `--session-id` or `CONTEXTLATTICE_SESSION_ID` to force a specific session. Set `CONTEXTLATTICE_AUTO_SESSION_DISABLED=1` to disable automatic session creation.

Canonical event families include `session.started`, `context_pack.completed`, `dream.completed`, `graph.neighbors_returned`, `graph.edge_touched`, `decision.made`, `test.ran`, `handoff.created`, `writeback.completed`, and `session.completed`.

## Download Installers

- macOS DMG: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-macOS-universal.dmg`
- Homebrew cask: `brew tap sheawinkler/contextlattice && brew install --cask contextlattice`
- Windows MSI: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-windows-x64.msi`
- Linux bundle: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-linux-bootstrap.tar.gz`

## Resource Profiles

| Profile | CPU | RAM | Storage |
| --- | --- | --- | --- |
| Lite core | `2-4` vCPU | `8-12 GB` | `25-80 GB` |
| Lite advanced | `4-6` vCPU | `12-16 GB` | `80-140 GB` |
| Full | `6-8` vCPU | `12-20 GB` | `100-180 GB` |

## Memory Graph

- `GET|POST /v1/memory/edges` persists explicit typed relationships.
- `POST /v1/memory/edges/backfill` audits or applies deterministic retroactive edges and opt-in same-project `inferred_related` scoring. It is dry-run by default.
- `POST /v1/memory/neighbors` returns explicit/inferred edge neighbors merged with semantic/topic neighbors.

```bash
./scripts/agent/memory-edge-backfill
./scripts/agent/memory-edge-backfill --include-inferred --min-confidence 0.90
./scripts/agent/memory-edge-backfill --write
./scripts/agent/memory-edge-inferred-retrofill --all-projects
./scripts/agent/memory-edge-inferred-retrofill --all-projects --profile exploratory
./scripts/agent/memory-edge-inferred-retrofill --all-projects --profile exploratory --write --confirm-retrofill ALL_PROJECTS
./scripts/agent/memory-edge-inferred-retrofill --project hermes-agent-ultra --corpus disk --profile exploratory
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
- Release notes:
  - `docs/releases/v3.4.10.md`
  - `docs/releases/v3.4.5.md`
  - `docs/releases/v3.4.2.md`
  - `docs/releases/v3.4.1.md`

## License

Business Source License 1.1 (`LICENSE`).
