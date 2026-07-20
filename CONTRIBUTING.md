# Contributing to ContextLattice

ContextLattice is the local-first intelligence layer that gives AI agents
durable continuity, explainable retrieval, portable context, and verified
learning across harnesses. The CLI is the primary interface; the dashboard,
HTTP, and MCP are companion surfaces.

## Quick Start

```bash
cp .env.example .env
gmake quickstart
contextlattice doctor --pretty
```

The public local runtime is account-free. A Compose v2-compatible runtime is
required. Use the deployment profile that fits the machine rather than enabling
every optional service by default.

## Development Contract

- Preserve the CLI-first product path.
- Keep the active request path in Go/Rust; Python is build, migration, audit,
  and development tooling, not a live application service.
- Keep public Apache-2.0 source, commercial BUSL-1.1 source, and private
  research/operations boundaries explicit.
- Never include personal paths, secrets, private docs, or customer data in a
  public change or test fixture.
- Prefer small, deterministic checks and bounded artifacts over narrative proof.

## Verification

Run the narrow checks for the files you changed first. Before requesting review,
run the relevant lane gate and report exact commands and results.

Useful entry points:

```bash
gmake env-check
gmake public-product-truth-audit
curl -fsS http://127.0.0.1:8075/health | jq
contextlattice doctor --pretty
```

Runtime or Rust changes require a full rebuild/restart before live claims.
Frontend changes require desktop and mobile verification, including console and
network errors.

## Pull Requests

- Keep the change focused and reviewable.
- Name the affected lane: public, commercial, or private development.
- Explain behavior, safety boundaries, rollback, and exact verification.
- Update docs, contracts, generated projections, and tests when an interface or
  product claim changes.
- Do not commit generated drift manually when a repository generator owns it.

## Bugs and Security

Open a bug report with a redacted doctor result, release/install channel,
deployment profile, exact reproduction, and observed behavior. Do not publish
secrets or vulnerability details; follow [SECURITY.md](SECURITY.md) instead.
