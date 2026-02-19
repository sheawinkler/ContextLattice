# Messaging Surface Expansion (OpenClaw-style)

Updated: 2026-02-18

## Decision

Treat channel-first UX as a **priority next-track** item.
Not required to ship current public beta, but high leverage for adoption and
for making the product tangible to non-operators.

## Why this matters

- Lowers activation friction: users interact through familiar channels.
- Increases daily engagement and memory capture opportunities.
- Makes retrieval quality gains visible in normal chat workflows.

## Scope proposal

### Phase 1 (near-term)

- Build a `message-ingress` adapter layer for:
  - Telegram
  - Discord
  - Slack
- Route inbound messages through orchestrator for:
  - retrieval before response
  - structured post-response memory writes
- Add per-channel topic path conventions and write throttles.

### Phase 2

- Add command grammar for explicit memory operations:
  - `/remember`, `/recall`, `/context`, `/forget`
- Add approval-gated action dispatch for high-risk commands.
- Add channel analytics dashboard cards (latency, hit-rate, memory yield).

### Phase 3

- Multi-channel identity mapping and preference continuity.
- Per-channel policy templates for retention and safety.
- Assistive operator controls (bulk replay, redact, export).

## Technical shape

- Keep orchestrator as control plane.
- Channel adapters only translate protocol + auth + formatting.
- Preserve existing sink fanout/backpressure/retention policy path.

## Guardrails

- Per-channel rate limits and debouncing.
- Content-size caps to avoid queue pressure.
- Explicit high-risk action gating remains mandatory.

## Launch readiness for this track

- Start with one channel MVP (Slack or Discord) behind feature flag.
- Require end-to-end traceability from inbound message to fanout sink outcome.
