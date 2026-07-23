---
title: Integrations
summary: Connect supported agent profiles, HTTP clients, and MCP consumers after proving the core runtime.
eyebrow: Connect surfaces
order: 5
slug: integrations
---
# Integrations

Integrate only after the runtime has passed its own health and readback checks. The CLI is the normal agent surface; HTTP and MCP exist for applications and harnesses that need programmatic access.

## Supported agent profiles

ContextLattice ships profile-aware templates for Codex, Claude Code, OpenCode, Hermes Agent, Hermes Ultra, OMP, Mercury, Pi, Droid, ChatGPT, and Claude surfaces.

```zsh
contextlattice_adopt profiles
contextlattice_adopt integrate \
  --repo . \
  --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,omp,mercury-agent,pi,droid \
  --pretty
```

The integration command writes bounded managed blocks to the instruction files each selected profile actually reads. It does not install the agent binaries.

## Agent operating contract

1. Retrieve scoped context before substantial planning or implementation.
2. Inspect provenance and degraded sources before acting.
3. Persist concise, verified checkpoints during long work.
4. Correct wrong, stale, useful, or superseded context explicitly.
5. Finish with the verified result and matching outcome.

The reusable public templates live in `docs/public_overview/templates/agents/`.

## HTTP base and authentication

The default local orchestrator is `http://127.0.0.1:8075`.

- `GET /health` is the liveness endpoint.
- `GET /status` requires `x-api-key`.
- Protected `/memory/*`, `/telemetry/*`, and operator routes require the configured API key.
- Callers should read secrets from local runtime configuration and never print them.

## Write and search smoke proof

```zsh
ORCH_KEY="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env)"

curl -fsS \
  -H "content-type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d '{"projectName":"_global","fileName":"smoke/integration.md","content":"integration smoke ok"}' \
  http://127.0.0.1:8075/memory/write | jq

curl -fsS \
  -H "content-type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d '{"query":"integration smoke ok","limit":3}' \
  http://127.0.0.1:8075/memory/search | jq
```

Treat a successful write plus attributable readback as stronger evidence than container state alone.

## Endpoint map

- `POST /memory/write` persists a checkpoint and reports fanout status.
- `POST /memory/search` returns scoped results, grounding, debug posture, and continuation metadata.
- `POST /memory/context-pack` builds a hierarchical context pack for complex work.
- `POST /memory/synthesis-pack` assembles high-signal findings and bridges over bounded evidence.
- `POST /v1/memory/neighbors` expands from a known memory record.
- `GET /memory/search/continuations/{token}/events` streams slow-source continuation events.
- `GET /v1/agents/sessions/{id}/rollup` returns bounded objective and evidence state.
- `GET /v1/agents/sessions/{id}/context-package` prepares the next-model handoff.

## MCP

The default memory MCP hub endpoint is `http://127.0.0.1:53130/memorymcp/mcp`. Full or operator stacks may expose additional MCP adapters. Keep orchestrator memory writes as the durable control plane even when the client retrieves through MCP.

## Continuations

When a search returns continuation metadata, consume its event stream or use the CLI inbox drain instead of immediately repeating the same deep query.

```zsh
contextlattice_async_inbox_drain --session-id <session-id>
contextlattice_async_inbox_hook --session-id <session-id>
```

The hook is fail-open and is appropriate only for runtimes with a supported post-tool boundary.

## Next

Read [Operations](operations.md) before changing runtime profiles, restarting services, or responding to degraded retrieval.
