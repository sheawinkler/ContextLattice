# A2A Readiness Profile

ContextLattice should track A2A compatibility without taking a hard runtime
dependency on the A2A SDK yet.

## Decision

- Keep ContextLattice's current Go/Python contract registry as the source of
  truth for local agent-boundary validation.
- Add `a2a_readiness_profile.v1` as a compatibility contract, not a transport
  implementation.
- Map A2A concepts to ContextLattice boundaries before exposing any public A2A
  server:
  - Agent Card -> advertised ContextLattice capability/profile metadata.
  - Task lifecycle -> `/agents/tasks` plus `agent_task_result.v1`.
  - Streaming/push -> future event bridge; do not bolt this onto writeback.
  - Opaque-agent collaboration -> no raw internal memory, prompts, tool calls,
    or secrets cross the boundary.

## Why Not Vendor A2A Now

A2A is useful because it standardizes opaque agent discovery, collaboration, and
task exchange. The stable ContextLattice move is to become compatible with the
shape first: explicit contracts, task lifecycle metadata, span/trajectory event
schemas, and telemetry around violations. Pulling in an SDK now would make the
dependency graph larger before ContextLattice has a proven A2A-facing endpoint.

## Spans And Flight Recorder

`project` and `topic_path` are semantic memory coordinates. They are not spans.

- `agent_span.v1` is the operation-level unit: agent, lane, endpoint, status,
  start/end time, and trace id.
- `agent_flight_recorder_event.v1` is the trajectory/event unit: compact
  decision/action/checkpoint records linked by trace id.
- Storage can initially reuse memory writeback under a telemetry or runbook
  topic, but the contract IDs keep the boundary distinct from normal project
  memory.

## Sources

- A2A repository: https://github.com/a2aproject/A2A
- A2A protocol specification: https://a2a-protocol.org/latest/specification/
