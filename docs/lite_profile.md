# Lite profile

The lite profile runs the smallest viable ContextLattice stack:

- Memory bank (Mongo + memorymcp-http)
- Qdrant + MCP Qdrant adapter
- MCP hub
- Orchestrator

MindsDB, Langfuse, Letta, and prompt tooling are disabled to keep the footprint small.

## Start

```bash
gmake mem-up-lite
```

## Stop

```bash
gmake mem-down-lite
```

## Status

```bash
gmake mem-ps-lite
```
