# Dockerfile Map (public/main)

This file documents the canonical Dockerfiles used by public `ContextLattice`.

## Core runtime

- `Dockerfile.orchestrator`: standalone gateway-go image used for Glama and single-container deployments.
- `Dockerfile.dashboard`: dashboard dev/runtime image.
- `Dockerfile.gateway-go`: gateway-go service image for compose stacks.
- `Dockerfile.orchestrator-go`: orchestrator-go service image for compose stacks.

## Retrieval / adapters

- `dockerfile`: qdrant MCP HTTP bridge image (legacy lowercase filename kept for compose compatibility).
- `Dockerfile.memorymcp`: memorymcp bridge image.
- `Dockerfile.fastembed`: fastembed sidecar image.
- `Dockerfile.memory-bank-spike-rs`: Rust memory-bank spike adapter image.
- `Dockerfile.lancedb-spike-adapter`: LanceDB spike adapter image.
- `Dockerfile.external-spike-adapter`: external spike adapter image.
- `Dockerfile.mindsdb-http-proxy`: MindsDB HTTP proxy image.

## Utility / optional

- `Dockerfile.gate-refresh`: gate refresh runner image.
- `Dockerfile.mcp-gateway`: lightweight MCP gateway helper image.
- `Dockerfile.qdrant_adv`: advanced qdrant MCP helper image.
- `Dockerfile.hf-lite`: Hugging Face lite deployment image.

## Notes

- `docker/mindsdb-http-proxy/Dockerfile` was removed because it duplicated
  `Dockerfile.mindsdb-http-proxy` and had no references in compose, scripts,
  docs, or build paths.
