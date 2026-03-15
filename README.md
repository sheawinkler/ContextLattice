# ContextLattice

<p align="center">
  <a href="https://contextlattice.io/" target="_blank" rel="noopener noreferrer">
    <img src="docs/public_overview/assets/architecture-service-map.svg" alt="Context Lattice system context map" width="100%" />
  </a>
</p>

<p align="center">
  Local-first memory orchestration for AI systems with durable writes, multi-sink fanout, retrieval learning loops, and operator-grade controls.
</p>

<p align="center">
  <a href="https://modelcontextprotocol.io/"><img src="https://img.shields.io/badge/MCP-HTTP%20Gateway-6b7280?style=for-the-badge" alt="MCP HTTP Gateway"></a>
  <a href="#quickstart"><img src="https://img.shields.io/badge/Deploy-Docker%20Compose-4b5563?style=for-the-badge" alt="Docker Compose"></a>
  <a href="#performance-profile"><img src="https://img.shields.io/badge/Write%20Rate-100%2B%20msg%2Fs-374151?style=for-the-badge" alt="Write rate"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-BSL%201.1-1f2937?style=for-the-badge" alt="BSL 1.1"></a>
</p>

<p align="center">
  <a href="https://contextlattice.io/">Overview</a> |
  <a href="https://contextlattice.io/architecture.html">Architecture</a> |
  <a href="https://contextlattice.io/roadmap.html">V3 Roadmap</a> |
  <a href="https://contextlattice.io/installation.html">Installation</a> |
  <a href="https://contextlattice.io/integration.html">Integrations</a> |
  <a href="https://contextlattice.io/troubleshooting.html">Troubleshooting</a> |
  <a href="https://contextlattice.io/updates.html">Updates</a>
</p>

## Why Context Lattice

Context Lattice is built for teams running high-volume memory writes where durability and retrieval quality matter more than prompt bloat.

- One ingress contract (`/memory/write`) with validated + normalized payloads.
- Durable outbox fanout to specialized sinks (Qdrant, Mongo raw, MindsDB, Letta, memory-bank).
- Retrieval orchestration that merges multi-source recall and improves ranking through a learning loop.
- Code-context enrichment + reranking (symbol overlap, file-path proximity, recency) behind env-gated controls.
- Local-first operation with optional cloud BYO for specific sinks.

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

## Quickstart

### Prerequisites

- Container app requirement: a Compose v2-compatible container runtime is required (`docker compose`), such as Docker Desktop, Docker Engine, or another runtime that supports Compose v2
- Supported host environments: macOS, Linux, or Windows (WSL2)
- Host machine sized for selected profile (`lite` vs `full`) with enough CPU, RAM, and disk
- `gmake`, `jq`, `rg`, `python3`, `curl`
- Tested baseline: macOS 13+ with Docker Desktop

### 1) Configure environment

```bash
cp .env.example .env
ln -svf ../../.env infra/compose/.env
```

Strict runtime lock (prevents tuning drift across restarts):

```bash
gmake env-lock-apply
gmake env-lock-check
```

`config/env/strict_runtime.env` is the single source of truth for critical runtime/tuning keys.
`gmake up`, `gmake mem-up`, and release/lite launch targets auto-apply this lock before compose starts.

Optional Letta backlog auto-prune tuning in `.env`:

```bash
LETTA_AUTO_PRUNE_ENABLED=true
LETTA_AUTO_PRUNE_INTERVAL_SECS=75
LETTA_AUTO_PRUNE_BACKLOG_TRIGGER=1000
LETTA_AUTO_PRUNE_LIMIT=20000
LETTA_AUTO_PRUNE_TIMEOUT_SECS=45
LETTA_AUTO_PRUNE_STATUSES=pending,retrying
```

Optional code-context and agent capability surfaces:

```bash
ORCH_CODE_CONTEXT_ENRICH_ENABLED=true
ORCH_MCP_CAPABILITY_MAP_ENABLED=true
ORCH_BROWSER_CONTEXT_INGEST_ENABLED=true
```

Optional fastembed-rs adapter spike (feature-flagged):

```bash
ORCH_ADAPTER_FASTEMBED_RS_ENABLED=true
ORCH_FASTEMBED_RS_BASE_URL=http://fastembed-rs:8080
ORCH_FASTEMBED_RS_ROUTE=/embed
ORCH_FASTEMBED_RS_TIMEOUT_SECS=2.5
```

When enabled, orchestrator Qdrant write fanout uses batched embeddings (`embed_text_batch`) to reduce per-item adapter overhead.

Optional lexical guard for staged retrieval (policy-aware slow-source deferral):

```bash
GO_RETRIEVAL_LEXICAL_GUARD_ENABLED=true
GO_RETRIEVAL_LEXICAL_GUARD_MIN_COVERAGE=0.55
GO_RETRIEVAL_LEXICAL_GUARD_MIN_RESULTS=1
```

Optional mode-aware Qdrant tuning:

```bash
ORCH_QDRANT_SEARCH_MODE_HNSW_EF={"fast":48,"balanced":96,"deep":128}
ORCH_QDRANT_SEARCH_MODE_LIMIT_CAPS={"fast":80,"balanced":120,"deep":180}
ORCH_QDRANT_FILTERLESS_LIMIT_CAP=96
ORCH_QDRANT_WARMUP_ENABLED=true
ORCH_QDRANT_WARMUP_DELAY_SECS=2
ORCH_QDRANT_WARMUP_TIMEOUT_SECS=20
```

Deep async durability + telemetry store routing:

```bash
ORCH_RECALL_DEEP_ASYNC_PERSIST_ENABLED=true
ORCH_RECALL_DEEP_ASYNC_STORE_BACKEND=mongo
ORCH_RECALL_DEEP_ASYNC_MONGO_DB=memmcp_raw
ORCH_RECALL_DEEP_ASYNC_MONGO_COLLECTION=recall_deep_async_jobs
ORCH_TELEMETRY_DB=memmcp_raw
ORCH_TELEMETRY_COLLECTION=retrieval_telemetry
ORCH_TELEMETRY_PERSIST_ENABLED=true
ORCH_RETRIEVAL_MEMORY_BANK_DEFAULT_ENABLED=false
ORCH_MEMORY_BANK_SEARCH_BACKEND=native
ORCH_MEMORY_BANK_SPIKE_HTTP_URL=
ORCH_MEMORY_BANK_SPIKE_SEARCH_ROUTE=/search
```

### 2) One-command quickstart (recommended)

```bash
gmake quickstart
```

This command:
- creates `.env` if missing
- links compose env
- generates `CONTEXTLATTICE_ORCHESTRATOR_API_KEY` if missing
- applies secure local defaults
- applies strict runtime tuning lock
- boots the stack
- runs smoke + auth-safe health checks

### 3) 60-second verify (recommended)

```bash
ORCH_KEY="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env)"

curl -fsS http://127.0.0.1:8075/health | jq
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/status | jq '.service,.sinks'
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/ops/capabilities | jq
```

Expected:
- `/health` returns `{"ok": true, ...}`
- `/status` returns service and sink states (with API key)

### 4) Manual bootstrap (optional)

```bash
BOOTSTRAP=1 scripts/first_run.sh
```

`MINDSDB_REQUIRED` now defaults automatically from `COMPOSE_PROFILES`.

### 5) Other launch profiles

```bash
# launch using current COMPOSE_PROFILES from .env
gmake mem-up

# explicit modes
gmake mem-up-lite
gmake mem-up-full
gmake mem-up-core

# persist profile mode for future gmake mem-up
gmake mem-mode-full
gmake mem-mode-core
```

### 6) Verify health and telemetry

```bash
ORCH_KEY="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env)"

curl -fsS http://127.0.0.1:8075/health | jq
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/status | jq
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/telemetry/fanout | jq
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/telemetry/fanout | jq '.lettaAutoPrune'
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/telemetry/retention | jq
curl -fsS -X POST -H "x-api-key: ${ORCH_KEY}" \
  "http://127.0.0.1:8075/telemetry/memory/cleanup-low-value/chunked?dry_run=true&project_batch=10&per_project_limit=250" | jq
curl -fsS -X POST -H "x-api-key: ${ORCH_KEY}" \
  "http://127.0.0.1:8075/telemetry/fanout/letta/auto-prune/run?force=false" | jq
curl -fsS -X POST -H "x-api-key: ${ORCH_KEY}" \
  "http://127.0.0.1:8075/maintenance/telemetry/purge?dry_run=true&include_qdrant=true&include_mindsdb=true&include_letta=true" | jq
```

### 7) First-run toggles (optional)

```bash
scripts/first_run.sh --allow-secrets-storage
scripts/first_run.sh --block-secrets-storage
scripts/first_run.sh --insecure-local
```

`scripts/first_run.sh` now enforces secure local-first defaults unless explicitly overridden:
- loopback-only host port binding (`HOST_BIND_ADDRESS=127.0.0.1`)
- production auth posture (`CONTEXTLATTICE_ENV=production`, strict API key requirement)
- private status/docs/webhook endpoints
- secrets-safe writes (`SECRETS_STORAGE_MODE=redact`)

Security toggles:
- `--allow-secrets-storage`
- `--block-secrets-storage`
- `--insecure-local` (explicit opt-out)

## Agent Operator Prompt (Paste Once)

Paste this into any new agent session (ChatGPT app, Claude chat apps, Claude Code, Codex):

```text
You must use Context Lattice as the memory/context layer.

Runtime:
- Orchestrator: http://127.0.0.1:8075
- API key: CONTEXTLATTICE_ORCHESTRATOR_API_KEY from my local .env

Required behavior:
1) Before planning, call POST /memory/search with compact query + project/topic filters.
2) During long tasks, checkpoint major decisions/outcomes via POST /memory/write.
3) Before final answer, run one more POST /memory/search for recency.
4) Keep writes compact (summary, decisions, diffs), never full transcripts.
5) If memory endpoints fail, continue task and report degraded-memory mode explicitly.
6) Use read-call timeouts that match retrieval mode:
   - fast: 25s
   - balanced: 60s
   - deep (or explicit `letta`/`memory_bank` sources): 75s
   Fast/balanced modes keep slow sources async by default unless explicitly requested (`sources=[...]`).
   Deep mode now defaults to async completion: you get immediate partial results plus `job_id`/`poll_url`/`events_url`, then fetch final results from `GET /memory/search/jobs/{job_id}` (or `/memory/search/async/{job_id}`) or stream updates from `GET /memory/search/jobs/{job_id}/events`.
   If a deep read returns partials, show those immediately and poll once after 5-15s for warmed slow-source completion.
```

Detailed playbook: `docs/human_agent_instruction_playbook.md`

## External Agent Task Routing (Generic)

Context Lattice can queue and route tasks to external runners (Codex, OpenCode, Claude Code) and still supports internal application workers.

- External-first pattern: set `agent` to the external runner id (`codex`, `opencode`, `claude-code`, or any custom worker name).
- Internal app workers remain supported: use `agent=internal` or leave unassigned (`agent` empty / `any`) for orchestrator workers.
- Practical default: external runners as primary path, internal workers as fallback/secondary for high-resource systems.

```bash
ORCH_KEY="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env)"

# 1) Create a task targeted to any external runner id.
curl -fsS -X POST http://127.0.0.1:8075/agents/tasks \
  -H "content-type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d '{
    "title":"summarize deployment notes",
    "project":"default",
    "agent":"codex",
    "priority":3,
    "payload":{
      "action":"memory_search",
      "query":"deployment notes",
      "project":"default",
      "limit":8
    }
  }'

# 2) Runner claims only tasks assigned to its worker id (plus unassigned/any tasks).
curl -fsS -X POST "http://127.0.0.1:8075/agents/tasks/next?worker=codex" \
  -H "x-api-key: ${ORCH_KEY}"

# 3) Runner reports completion.
curl -fsS -X POST http://127.0.0.1:8075/agents/tasks/<TASK_ID>/status \
  -H "content-type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d '{"status":"succeeded","message":"completed by external runner","metadata":{"worker":"codex"}}'
```

## Performance Profile

- Sustained write throughput target: `100+ messages/second` for typical memory payloads on modern laptop-class hardware.
- Outbox protection: fanout retries, coalescing windows, and target-level backpressure to protect core durability.
- Storage pressure controls: retention runner, low-value TTL pruning, optional snapshot pruning, and external NVMe cold path support.
- Retrieval path: parallel source reads with orchestrator merge/rank loop and preference-learning feedback.
- Telemetry routing guards (default-on): telemetry-like writes are filtered out of `qdrant`/`mindsdb`/`letta` fanout.
- Memory-bank policy: default non-critical source (`ORCH_RETRIEVAL_MEMORY_BANK_DEFAULT_ENABLED=false`) with explicit opt-in and optional non-ANE spike backend lane.

Telemetry routing/cleanup toggles:

```bash
ORCH_MEMORY_BANK_TELEMETRY_GUARD_ENABLED=true
ORCH_MEMORY_BANK_TELEMETRY_TOPIC_PREFIXES=telemetry,metrics,signals,overrides
ORCH_MEMORY_BANK_TELEMETRY_MARKERS=telemetry,metrics,__state__,__stats__,__snapshots__,__health__,__allocations__,_agg-,queue__
ORCH_QDRANT_TELEMETRY_GUARD_ENABLED=true
ORCH_MINDSDB_TELEMETRY_GUARD_ENABLED=true
ORCH_LETTA_TELEMETRY_GUARD_ENABLED=true
MINDSDB_LOW_VALUE_RETENTION_HOURS=48
```

### v2.0.0 Runtime Comparison (v1 legacy vs v2 cutover)

Live A/B benchmark on `POST /memory/search` using `bench/phase1_runtime_comparison.py` with `8` requests and `20s` timeout:

- v2 cutover (`USE_RUST_* = true`, `USE_GO_ORCHESTRATOR = true`):
  - mean `3557ms`, p50 `2334ms`, p95 `8494ms`, errors `0/8`
- v1-style legacy path (`USE_RUST_* = false`, `USE_GO_ORCHESTRATOR = false`):
  - mean `17565ms`, p50 `20006ms`, p95 `20008ms`, errors `7/8` (timeouts)
- Observed improvement:
  - mean `4.94x` faster (about `5x`)
  - p50 `8.57x` faster
  - p95 `2.36x` faster

Artifacts:
- `bench/results/phase1_ab_rustgo_on_fast_20260304T182812Z.json`
- `bench/results/phase1_ab_rustgo_off_fast_20260304T182916Z.json`

## V3 Roadmap (Issues 68-72)

V3 is focused on application efficacy, not speed in isolation:

- lower deep-read p95/p99 tails and timeout rates
- higher recall quality for agent decisions
- stronger runner interoperability and task-lifecycle visibility
- ANE sidecar acceleration path (M-series macOS) with automatic fallback

Roadmap documents:
- full plan: `docs/v3-roadmap.md`
- public roadmap page: `https://contextlattice.io/roadmap.html`

Program graph:

```text
V3 Objective: Context Efficacy at Scale
  ├─ Track A (Issues #69 + #72): performance + deep-read stability
  ├─ Track B (Issues #70 + #72): recall quality + memory semantics
  └─ Track C (Issues #68 + #71): runner interop + compute backend
      -> unified security/benchmark/recall gates -> staged cutover
```

## Migration Runtime (Phases 1-8)

The orchestrator now runs Rust+Go as the default runtime path. Python remains in place as a legacy fallback when a proxy is unavailable.

- Runtime interfaces: `Codec`, `MemoryStore`, `Retriever`, `Scheduler`, `StateDelta`
- Status endpoint: `GET /migration/runtime`
- Flags:
  - `USE_RUST_CODEC`
  - `USE_RUST_MEMORY`
  - `USE_RUST_RETRIEVAL`
  - `ORCH_RUST_RETRIEVAL_VECTOR_BACKEND` (`auto|qdrant_remote|usearch_ann`)
  - `ORCH_RUST_RETRIEVAL_LEXICAL_BACKEND` (`auto|none|tantivy_lexical`)
  - `ORCH_RUST_RETRIEVAL_BACKEND_STRICT`
  - `GO_RETRIEVAL_LEXICAL_GUARD_ENABLED`
  - `GO_RETRIEVAL_LEXICAL_GUARD_MIN_COVERAGE`
  - `GO_RETRIEVAL_LEXICAL_GUARD_MIN_RESULTS`
  - `USE_GO_ORCHESTRATOR`
  - `CONTEXTLATTICE_ENGINE_MODE` (`embedded` or `service`)
  - `CONTEXTLATTICE_ENGINE_URL`
  - `CONTEXTLATTICE_GO_ORCHESTRATOR_URL`
  - `MIGRATION_SHADOW_DUAL_RUN`
  - `MIGRATION_CANARY_ENABLED`

Migration scaffolding:

- Rust crates: `crates/context_codec`, `crates/context_engine`, `crates/context_retrieval`
- Service contract: `proto/contextlattice_engine.proto`
- Go services: `services/orchestrator-go`, `services/gateway-go`
- API docs: `docs/engine-api.md`, `docs/migration-phase-status.md`

Default cutover toggles:

```bash
USE_RUST_CODEC=true
USE_RUST_MEMORY=true
USE_RUST_RETRIEVAL=true
USE_GO_ORCHESTRATOR=true
CONTEXTLATTICE_ENGINE_MODE=service
CONTEXTLATTICE_ENGINE_URL=http://contextlattice-orchestrator:8075
CONTEXTLATTICE_GO_ORCHESTRATOR_URL=http://orchestrator-go:8090
MIGRATION_SHADOW_DUAL_RUN=true
MIGRATION_CANARY_ENABLED=true
```

Rollback/legacy toggles (temporary fallback only):

```bash
USE_RUST_CODEC=false
USE_RUST_MEMORY=false
USE_RUST_RETRIEVAL=false
USE_GO_ORCHESTRATOR=false
```

Pathway cache backend modes:

- `ORCH_RETRIEVAL_PATHWAY_CACHE_BACKEND=memory` (in-memory only)
- `ORCH_RETRIEVAL_PATHWAY_CACHE_BACKEND=redis` (read/write Redis backend)
- `ORCH_RETRIEVAL_PATHWAY_CACHE_BACKEND=redis_mirror` (write-through mirror only; read path stays in-memory)

## Model Runtime

- Ships with a sane local default (`qwen` via Ollama).
- Any OpenAI-compatible endpoint can be used when preferred.
- BYO model runtimes supported through:
  - Ollama
  - LM Studio
  - llama.cpp compatible server
  - hosted OpenAI-compatible providers

## Security defaults

- `SECRETS_STORAGE_MODE=redact` redacts secret-like material before memory persistence/fanout.
- `SECRETS_STORAGE_MODE=block` rejects writes containing secret-like material (`422`).
- `SECRETS_STORAGE_MODE=allow` stores write payloads as-is (operator opt-in).
- Compose host bindings default to loopback via `HOST_BIND_ADDRESS=127.0.0.1`.
- Production strict mode requires `CONTEXTLATTICE_ORCHESTRATOR_API_KEY`.

### Main branch release gate

Enforce PR-only merges on `main` with CODEOWNERS approval (`.github/CODEOWNERS` is `* @sheawinkler`):

```bash
scripts/enable_main_branch_protection.sh main 1
```

If GitHub returns `Upgrade to GitHub Pro or make this repository public`, switch repo visibility or plan, then rerun the command.

## Web 3 Ready

- IronClaw can be enabled as an optional messaging surface without changing the core local-first deployment.
- OpenClaw/ZeroClaw surfaces now run with strict secret-leakage protections by default.
- IronClaw docs and architecture conventions are excellent references for operator-facing completeness.

```bash
# optional IronClaw bridge
IRONCLAW_INTEGRATION_ENABLED=true
IRONCLAW_DEFAULT_PROJECT=messaging

# strict secret guard for openclaw/zeroclaw/ironclaw messaging surfaces
MESSAGING_OPENCLAW_STRICT_SECURITY=true
```

Ingress endpoints:
- `POST /integrations/messaging/openclaw`
- `POST /integrations/messaging/ironclaw`
- `POST /integrations/messaging/command`
- `@ContextLattice task create|status|list|approve|replay|deadletter|runtime`

## API Surface (selected)

- `POST /memory/write`
- `POST /memory/search`
- `POST /integrations/messaging/command`
- `POST /integrations/messaging/openclaw`
- `POST /integrations/messaging/ironclaw`
- `POST /integrations/telegram/webhook`
- `POST /integrations/slack/events`
- `POST /agents/tasks`
- `GET /agents/tasks`
- `GET /agents/tasks/runtime`
- `GET /agents/tasks/deadletter`
- `POST /agents/tasks/{task_id}/replay`
- `POST /agents/tasks/recover-leases`
- `GET /telemetry/memory`
- `GET /telemetry/fanout`
- `POST /telemetry/fanout/letta/auto-prune/run`
- `GET /telemetry/retention`
- `POST /telemetry/retention/run`
- `POST /maintenance/telemetry/purge`

## Docs Index

- Phase 0 performance baseline: `docs/perf-baseline.md`
- Migration plan: `docs/migration-plan.md`
- Migration interfaces (Phase 1 proposal): `docs/migration-interfaces.md`
- Benchmark harness docs: `bench/README.md`
- Public overview site source: `docs/public_overview/README.md`
- Legal and licensing: `docs/legal/README.md`

Pre-submit verifier:

```bash
gmake submission-preflight
python3 scripts/submission_preflight.py --online
gmake launch-lock
gmake launch-lock-public
```

## Private/Public Sync Notes

This repository (`sheawinkler/ContextLattice`) is the primary codebase.
Public landing collateral publishes from `sheawinkler/ContextLattice` branch `gh-pages`.

- Source: `docs/public_overview/`
- Sync script: `scripts/sync_public_overview.sh`
- Primary URL: `https://contextlattice.io/`
- Fallback URL: `https://sheawinkler.github.io/ContextLattice/`
- Historical mirror repository `sheawinkler/memmcp-overview` is archived and not used for live hosting.

## License

Business Source License 1.1 with change-date transition to Apache-2.0.
See `LICENSE` and `docs/legal/README.md`.
