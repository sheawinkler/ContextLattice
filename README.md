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

## Public Runtime Stack (v3)

- Ingress: `gateway-go`.
- Core memory + retrieval lanes: Go + Rust services.
- Degradation policy: fail-open retrieval with continuation lifecycle.
- Tooling compatibility: MCP + HTTP clients.
- Single-container lite builds (`Dockerfile.hf-lite`) also run `gateway-go` (no Python runtime dependency).

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
```

## Model Runtime

Task inference defaults to `ORCH_INFER_PROVIDER=auto`. `gateway-go` detects the host profile and probes local backends before selecting a route.

- Apple Silicon priority: `vllm-metal`, `mlx`, `ane_sidecar`, `llama-cpp`, `ollama`.
- CUDA/ROCm priority: `vllm`, `openai-compatible`, `llama-cpp`, `lmstudio`, `ollama`.
- CPU priority: `openai-compatible`, `llama-cpp`, `lmstudio`, `ollama`.
- Supported provider ids: `vllm`, `vllm-metal`, `mlx`, `mtplx`, `openai-compatible`, `lmstudio`, `llama-cpp`, `ane_sidecar`, `ollama`.

Inspect live routing and benchmark configured backends:

```bash
scripts/inference_runtime_policy.sh
scripts/benchmark_inference_backends.sh
```

Embedding defaults to the Rust `fastembed-rs` sidecar. Ollama stays available as an explicit compatibility fallback, not the preferred embedding path.

## Agent CLI

Installer and quickstart paths install agent helpers under `~/.contextlattice/bin`.

```bash
contextlattice_agent_start --soft --compact
contextlattice_search -h
contextlattice_write -h
contextlattice_checkpoint -h
```

- `contextlattice_agent_start` runs the lightweight startup guard for agents.
- `contextlattice_checkpoint` writes a checkpoint and verifies readback.
- Hook pack details: `docs/agent-hooks.md`.

## Download Installers

- macOS DMG: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-macOS-universal.dmg`
- Windows MSI: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-windows-x64.msi`
- Linux bundle: `https://github.com/sheawinkler/ContextLattice/releases/latest/download/ContextLattice-linux-bootstrap.tar.gz`

## Resource Profiles

| Profile | CPU | RAM | Storage |
| --- | --- | --- | --- |
| Lite | `2-4` vCPU | `8-12 GB` | `25-80 GB` |
| Full | `6-8` vCPU | `12-20 GB` | `100-180 GB` |

## Security and Privacy

- Local-first by default.
- API-key protected operational routes.
- Secret-like content redaction controls.
- Premium billing/provider route maps are intentionally kept out of public docs.

## Docs

- Overview: `https://contextlattice.io/`
- Architecture: `https://contextlattice.io/architecture.html`
- Wiki: `https://contextlattice.io/wiki.html`
- Installation: `https://contextlattice.io/installation.html`
- Integrations: `https://contextlattice.io/integration.html`
- Troubleshooting: `https://contextlattice.io/troubleshooting.html`
- Updates: `https://contextlattice.io/updates.html`

## License

Business Source License 1.1 (`LICENSE`).
