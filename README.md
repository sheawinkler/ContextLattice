# ContextLattice

<p align="center">
  <a href="https://contextlattice.io/" target="_blank" rel="noopener noreferrer">
    <img src="docs/public_overview/assets/architecture-service-map.svg" alt="ContextLattice architecture" width="100%" />
  </a>
</p>

<p align="center">
  Local-first memory and context orchestration for agents and apps
</p>

<p align="center">
  <a href="https://modelcontextprotocol.io/"><img src="https://img.shields.io/badge/MCP-HTTP%20Gateway-6b7280?style=for-the-badge" alt="MCP HTTP Gateway"></a>
  <a href="#quickstart"><img src="https://img.shields.io/badge/Deploy-Docker%20Compose-4b5563?style=for-the-badge" alt="Docker Compose"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache%202.0-1f2937?style=for-the-badge" alt="Apache 2.0"></a>
</p>

[![context-lattice MCP server](https://glama.ai/mcp/servers/sheawinkler/context-lattice/badges/card.svg?v=20260407)](https://glama.ai/mcp/servers/sheawinkler/context-lattice)

<p align="center">
  <a href="https://contextlattice.io/">Overview</a> |
  <a href="https://contextlattice.io/architecture.html">Architecture</a> |
  <a href="https://contextlattice.io/installation.html">Installation</a> |
  <a href="https://contextlattice.io/integration.html">Integrations</a> |
  <a href="https://contextlattice.io/troubleshooting.html">Troubleshooting</a> |
  <a href="https://contextlattice.io/updates.html">Updates</a>
</p>

## Why ContextLattice

ContextLattice reduces repeated inference by turning prior project work into high-signal, retrievable context.

- Durable memory writes with fanout to specialized stores.
- Fast + deep retrieval modes with staged fetch and fail-open continuation.
- Rollup-first context to keep token use efficient while preserving drill-down paths to raw artifacts.
- Local-first deployment with optional cloud-backed dependencies.
- Human + agent UX through HTTP APIs, MCP transport, and operations dashboard.

## Architecture (Public v3 lane)

| Layer | Primary runtime | Responsibility |
| --- | --- | --- |
| Gateway/API | Go | `/memory/*` + `/v1/*` orchestration, staged retrieval policy, continuation lifecycle |
| Retrieval + memory services | Go + Rust | Fast/durable retrieval lanes, rollup handling, memory-bank adapters |
| Legacy fallback | Python | Compatibility fallback only (not default hot path) |
| Dashboard | TypeScript/Next.js | Console, mindmap, status, billing, setup UX |

## Install

### Less-technical installers

- macOS DMG: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-macOS-universal.dmg`
- Linux bundle: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-linux-bootstrap.tar.gz`
- Windows MSI: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-windows-x64.msi`

### Developer install

```bash
git clone git@github.com:sheawinkler/ContextLattice.git
cd ContextLattice
gmake quickstart
```

## Quickstart

### Prerequisites

- Docker/Compose v2-compatible runtime
- macOS, Linux, or Windows (WSL2)
- `gmake`, `jq`, `rg`, `python3`, `curl`

### Launch

```bash
cp .env.example .env
ln -svf ../../.env infra/compose/.env
gmake quickstart
```

`gmake quickstart` prompts for a runtime profile and launches with sensible defaults.

### Verify

```bash
ORCH_KEY="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env)"

curl -fsS http://127.0.0.1:8075/health | jq
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/status | jq '.service,.sinks'
```

## Runtime Profiles

| Profile | Use case | CPU | RAM | Storage |
| --- | --- | --- | --- | --- |
| `lite` | Laptop-friendly local usage | 2-4 vCPU | 8-12 GB | 25-80 GB |
| `full` | Higher throughput and deeper recall | 6-8 vCPU | 12-20 GB | 100-180 GB |

## Core API examples

### Write memory

```bash
curl -X POST "http://127.0.0.1:8075/memory/write" \
  -H "Content-Type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d '{
    "projectName": "my_project",
    "fileName": "notes/decision.md",
    "content": "Switched retrieval_mode to balanced for normal runs.",
    "topicPath": "runbooks/retrieval"
  }'
```

### Read memory

```bash
curl -X POST "http://127.0.0.1:8075/memory/search" \
  -H "Content-Type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d '{
    "project": "my_project",
    "query": "retrieval mode decision",
    "topic_path": "runbooks/retrieval",
    "include_grounding": true
  }'
```

### Deep read with continuation metadata

```bash
curl -X POST "http://127.0.0.1:8075/memory/search" \
  -H "Content-Type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d '{
    "project": "my_project",
    "query": "full architecture context",
    "retrieval_mode": "deep",
    "include_grounding": true,
    "include_retrieval_debug": true
  }'
```

## Configuration (public-safe essentials)

Set only what you need for normal operation:

```bash
CONTEXTLATTICE_ORCHESTRATOR_URL=http://127.0.0.1:8075
CONTEXTLATTICE_ORCHESTRATOR_API_KEY=<set-by-setup>
NEXTAUTH_URL=http://localhost:3000
NEXTAUTH_SECRET=<long-random-secret>
APP_URL=http://localhost:3000
```

For full config reference, use `.env.example`.

## Dashboard

- UI: `http://127.0.0.1:3000/console`
- Mindmap: `http://127.0.0.1:3000/mindmap`
- Status: `http://127.0.0.1:3000/status`

## Public vs paid

This repository tracks the public free lane (`v3.x`).
Advanced premium tuning, proprietary optimization policy, and private commercialization docs live outside this public lane.

## Documentation

- Website docs: `https://contextlattice.io/`
- Local docs index: `docs/`
- Hugging Face lite deployment: `docs/huggingface-space-lite.md`

## License

Apache 2.0. See [LICENSE](LICENSE).
