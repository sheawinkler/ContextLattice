---
title: Core concepts
summary: A concise model of continuity, scoped retrieval, evidence, continuation, corrections, and storage lanes.
eyebrow: Mental model
order: 3
slug: concepts
---
# Core concepts

ContextLattice sits between agent harnesses and durable context. Models still reason and harnesses still act; ContextLattice preserves the mission, selects relevant evidence, carries bounded state forward, and records what actually worked.

## The five product pillars

- **Durable continuity** preserves objectives, decisions, constraints, and verified checkpoints across sessions.
- **Explainable retrieval** returns evidence and scope, not an unexplained text blob.
- **Portable context** lets the same mission move between supported harnesses without transcript replay.
- **Verified skill evolution** records outcomes and corrections so reusable behavior can improve under evidence.
- **Aggregate Signal** remains a controlled preview gated by independent privacy and utility review.

## Context is not a transcript

Raw conversation history is large, noisy, and often stale. A useful context packet is a bounded reconstruction of what matters now:

- the current objective;
- relevant evidence and provenance;
- constraints and risks;
- unresolved questions;
- likely next actions;
- explicit uncertainty.

This shape gives the receiving agent something it can inspect rather than something it must blindly trust.

## Projects, topics, and sessions

A **project** is the durable top-level scope used by the normal CLI lifecycle. A **topic path** narrows memory inside that project. A **session** tracks the live objective and its retrieval, checkpoint, correction, and completion events.

Use stable project names. Create narrower topics when a repository, incident, release, or customer thread would otherwise collide with unrelated work.

## Retrieval modes

Start with the least expensive mode that can answer the task.

1. **Fast** favors the fast-now sources for narrow engineering loops.
2. **Balanced** mixes immediate results with measured continuation and is the normal broad-read posture.
3. **Deep** widens recall for audits, migrations, and root-cause work.

Slow sources may continue after the first bounded answer. Continuation tokens and events let clients receive that depth without making every interaction wait for the slowest dependency.

## Liveness, readiness, and real retrieval

These are separate claims:

- `/health` says the orchestrator can answer a liveness request.
- Authenticated status and doctor output describe dependency and configuration posture.
- A bounded write/search or context-pack proof demonstrates the actual retrieval path.

A healthy container is useful evidence, but it is not a substitute for a real readback when retrieval correctness matters.

## Evidence and provenance

High-value records carry enough context to be checked later: source identity, project, topic, file or artifact, timestamps, and the command or observation that produced the conclusion. Memory without provenance should be treated as a lead, not a confirmed fact.

## Outcomes and corrections

`contextlattice finish` reports whether the task succeeded, required repair, or failed. `contextlattice correct` labels retrieved context as useful, wrong, stale, or superseded. These signals close the loop without pretending that model confidence is ground truth.

## Local-first storage posture

The public local-lite core uses topic rollups and Qdrant. Full and operator stacks can add more sources behind the same agent-facing contract. Storage selection does not change the basic rule: preserve the durable record, keep retrieval scoped, and expose degraded sources rather than manufacturing certainty.

## Next

Use the [CLI reference](cli.md) for the normal workflow, or move to [Integrations](integrations.md) when an application needs HTTP or MCP.
