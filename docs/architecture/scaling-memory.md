# Scaling Memory as Data Grows

ContextLattice is local-first memory infrastructure built around one belief: agents should not get more forgetful as the work gets larger.

A chat transcript is a room. ContextLattice is the city map, the archive, and the transit system beneath the room. It keeps the fast memories close, routes deeper recall through specialized stores, preserves the write trail, exposes agent capabilities through the Skills Index, and gives tomorrow's agent enough provenance to pick up the thread without asking the user to replay the whole movie.

## The Scaling Shape

Public local lite starts simple:

- `topic_rollups` keep fresh project context close to the agent.
- `qdrant` gives the public local stack a vector-native recall lane.
- HTTP and MCP endpoints give workspaces and agents one shared contract.
- global CLI wrappers make start, search, and checkpoint flows repeatable.
- Skills Index search keeps low-probability capabilities discoverable without loading every skill into the active prompt.

As work grows, the same contract can fan out into deeper lanes:

- `postgres_pgvector` for SQL-adjacent vector workloads and structured deployment patterns.
- raw ledger storage for durable write truth and auditability.
- async continuation lanes for slower, deeper recall that should not block the first answer.
- graph and edge maintenance so memory becomes connected instead of becoming a pile of notes.
- learning feedback and behavior provenance so recall quality improves without losing the trail behind important decisions.

## Why This Matters

Long-context stuffing looks powerful until it becomes a tax. It burns tokens, buries the useful facts, and asks every agent to become its own half-broken librarian.

ContextLattice changes the shape of the problem:

1. Write once through a durable memory contract.
2. Retrieve only the context that matters now.
3. Let slow sources warm in the background.
4. Keep decisions, evidence, and task state reusable across tools and days.
5. Add deeper stores without changing the way agents plug in.

## Runtime Principles

- Correctness before cleverness: memory writes must be durable before they are fancy.
- Fast lane first: topic rollups and vector recall should answer quickly.
- Deep lane second: slow recall should enrich the work without freezing the operator.
- Bounded context always: the output to an agent should be useful, compact, and task-scoped.
- Local-first by default: users can run the memory layer on their own machine and choose which stores to enable.

## Public Product Boundary

The public story is the architecture, not the workshop. Public docs should explain how the memory fabric scales, which stores participate, and how agents integrate. Non-public rollout records, deployment secrets, and operating procedures stay out of the public repo.
