# Context Lattice (memMCP)

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

- Docker Desktop (Compose v2)
- `gmake`, `jq`, `rg`, `python3`, `curl`
- macOS 13+ (primary test environment)

### 1) Configure environment

```bash
cp .env.example .env
ln -svf ../../.env infra/compose/.env
```

### 2) One-command quickstart (recommended)

```bash
gmake quickstart
```

This command:
- creates `.env` if missing
- links compose env
- generates `MEMMCP_ORCHESTRATOR_API_KEY` if missing
- applies secure local defaults
- boots the stack
- runs smoke + auth-safe health checks

### 3) 60-second verify (recommended)

```bash
ORCH_KEY="$(awk -F= '/^MEMMCP_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env)"

curl -fsS http://127.0.0.1:8075/health | jq
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/status | jq '.service,.sinks'
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
ORCH_KEY="$(awk -F= '/^MEMMCP_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env)"

curl -fsS http://127.0.0.1:8075/health | jq
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/status | jq
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/telemetry/fanout | jq
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/telemetry/retention | jq
```

### 7) First-run toggles (optional)

```bash
scripts/first_run.sh --allow-secrets-storage
scripts/first_run.sh --block-secrets-storage
scripts/first_run.sh --insecure-local
```

`scripts/first_run.sh` now enforces secure local-first defaults unless explicitly overridden:
- loopback-only host port binding (`HOST_BIND_ADDRESS=127.0.0.1`)
- production auth posture (`MEMMCP_ENV=production`, strict API key requirement)
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
- API key: MEMMCP_ORCHESTRATOR_API_KEY from my local .env

Required behavior:
1) Before planning, call POST /memory/search with compact query + project/topic filters.
2) During long tasks, checkpoint major decisions/outcomes via POST /memory/write.
3) Before final answer, run one more POST /memory/search for recency.
4) Keep writes compact (summary, decisions, diffs), never full transcripts.
5) If memory endpoints fail, continue task and report degraded-memory mode explicitly.
```

Detailed playbook: `docs/human_agent_instruction_playbook.md`

## External Agent Task Routing (Generic)

Context Lattice can queue and route tasks to external runners (Codex, OpenCode, Claude Code) and still supports internal application workers.

- External-first pattern: set `agent` to the external runner id (`codex`, `opencode`, `claude-code`, or any custom worker name).
- Internal app workers remain supported: use `agent=internal` or leave unassigned (`agent` empty / `any`) for orchestrator workers.
- Practical default: external runners as primary path, internal workers as fallback/secondary for high-resource systems.

```bash
ORCH_KEY="$(awk -F= '/^MEMMCP_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env)"

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
- Production strict mode requires `MEMMCP_ORCHESTRATOR_API_KEY`.

### Main branch release gate (v1.0.0)

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
- `GET /telemetry/retention`
- `POST /telemetry/retention/run`

## Docs Index

- Runbook: `docs/onprem_full_runbook.md`
- Performance: `docs/performance.md`
- Retention operations: `docs/retention_ops.md`
- Storage controls: `docs/storage_and_retention.md`
- Orchestrator enhancements: `docs/orchestrator_enhancements.md`
- Launch checklist: `docs/launch_checklist.md`
- Submission requirements audit: `docs/submission_requirements.md`
- Human agent instruction playbook: `docs/human_agent_instruction_playbook.md`
- Public messaging package: `docs/public_messaging_package.md`
- Legal and licensing: `docs/legal/README.md`
- New repo migration plan: `docs/contextlattice_repo_migration_plan.md`

Pre-submit verifier:

```bash
gmake submission-preflight
python3 scripts/submission_preflight.py --online
```

## Private/Public Sync Notes

This repository (`sheawinkler/ContextLattice`) is the primary codebase.
Public landing collateral remains mirrored in `sheawinkler/memmcp-overview`.

- Source: `docs/public_overview/`
- Sync script: `scripts/sync_public_overview.sh`

## License

Business Source License 1.1 with change-date transition to Apache-2.0.
See `LICENSE` and `docs/legal/README.md`.
