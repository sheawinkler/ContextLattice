# Context Lattice Agent Skills Guide

Use these operating rules when integrating any agent with Context Lattice so retrieval quality improves over time without causing storage or queue pressure.

## 1) Write policy
- Write memory only for durable state transitions, decisions, and summarized outcomes.
- Do not write full raw chat transcripts unless explicitly required.
- Keep `summary` concise and task-relevant; include project, intent, and outcome.
- Use stable file paths and topic paths to preserve retrieval locality.
- For high-frequency streams, emit periodic rollups instead of per-event verbose writes.

## 2) Retrieval policy
- Retrieve context before planning and before final response generation.
- Prefer federated retrieval through orchestrator (`/memory/search`) over sink-specific direct reads.
- Pass project/topic filters whenever available.
- Treat returned source confidence as ranking guidance, not absolute truth.

## 3) Learning loop policy
- Capture user/operator feedback after outcomes and store it.
- Reinforce successful retrieval paths by writing compact post-task retrospectives.
- Record mistakes and corrections as explicit memory events so rerank quality can improve.

## 4) Throughput + storage policy
- Respect backpressure: if fanout is degraded, continue critical writes and avoid noisy low-value writes.
- Deduplicate near-identical writes from polling loops.
- Prefer local-first Qdrant and enable cloud only when required.
- Monitor `/telemetry/fanout` and `/telemetry/retention` during heavy workloads.

## 5) Minimal integration contract
- Orchestrator base: `http://127.0.0.1:8075`
- MCP hub endpoint: `http://127.0.0.1:53130/mcp`
- Required flow:
  - `POST /memory/search` before major reasoning steps
  - `POST /memory/write` after meaningful state changes
  - `GET /status` and `GET /telemetry/fanout` for runtime health

## 6) Agent-specific notes
- GPT apps: write compact memory checkpoints and avoid oversized payloads.
- Claude apps: use MCP hub tools and orchestrator retrieval before long responses.
- Claude Code / Codex: checkpoint memory after significant code edits, reviews, and merges.
- OpenClaw / ZeroClaw: map recall/save traits to orchestrator search/write endpoints with orchestrator as the control plane.

## 7) Messaging commands (human interface)
- Preferred command handle is `@ContextLattice`.
- Use `remember`, `recall`, and `status` for lightweight operator interactions.
- Route OpenClaw/ZeroClaw messenger traffic through `POST /integrations/messaging/openclaw`.
- Use directives in message text for routing precision: `project=<name> topic=<path> limit=<n>`.

## 8) Storage discipline for long-running agents
- Keep source repos local for low-latency edit loops.
- Keep high-growth service data on external NVMe when available.
- Before rehydrate/backfill runs, verify free disk and queue depth telemetry.
