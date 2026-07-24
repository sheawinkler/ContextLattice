---
title: Documentation
summary: The public field manual for installing, operating, and integrating ContextLattice without losing the thread.
eyebrow: Start here
order: 1
---
# ContextLattice documentation

ContextLattice is the local-first intelligence layer that gives AI agents durable continuity, explainable retrieval, portable context, and verified learning across harnesses.

This manual is generated from the Markdown in `docs/wiki`. Every page links back to its repository source, and every route is stable enough to bookmark, share, or open directly in an agent.

## Choose your path

- [Install and verify the runtime](getting-started.md) if this is your first machine.
- [Understand the operating model](concepts.md) before tuning retrieval or storage.
- [Use the primary CLI](cli.md) for the normal context lifecycle.
- [Connect an agent or application](integrations.md) after the core runtime is healthy.
- [Operate and recover the stack](operations.md) with bounded checks.
- [Diagnose a failure](troubleshooting.md) from the exact failing boundary.
- [Review release posture](releases.md) before upgrading or publishing.

## The shortest useful loop

```zsh
contextlattice context "what must I know before this task?" --project my-project --pretty
contextlattice remember "verified checkpoint" --project my-project --pretty
contextlattice finish "work completed and tested" --project my-project --success --pretty
```

Use `contextlattice resume --project my-project --pretty` when a later session needs the bounded objective, evidence, risks, and next action. Use `contextlattice correct` when retrieved context is wrong, stale, useful, or superseded.

## Interface order

1. **CLI** is the primary human and agent interface.
2. **Dashboard** is the visibility, proof, account, and operations companion.
3. **HTTP and MCP** are integration surfaces for harnesses and applications.

The command line keeps the normal workflow explicit. Programmatic clients should use the same scoped lifecycle and avoid replaying full transcripts as memory.

## What good context looks like

- Scoped to a project, topic, task, or session.
- Small enough to inspect before acting.
- Grounded in source paths, timestamps, identities, or command evidence.
- Honest about missing or degraded sources.
- Correctable when evidence changes.
- Finished with a verified outcome so retrieval quality can improve.

> ContextLattice should make an agent more continuous, not more credulous. Retrieved records are evidence to inspect, never authority to expand a task or override current instructions.

## Product and support links

- [Public repository](https://github.com/sheawinkler/ContextLattice)
- [Dashboard](https://app.contextlattice.io/console)
- [Pricing](https://contextlattice.io/premium.html)
- [Release updates](https://contextlattice.io/updates.html)
- [Issue tracker](https://github.com/sheawinkler/ContextLattice/issues)
