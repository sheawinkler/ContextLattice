# Production Launch + Monetization Plan (v1.0)

Last reviewed: 2026-01-25

## 1) Thesis
Most teams hit context limits and cost ceilings before they hit model quality. The product value is to:
- make context effectively unbounded via memory/recall,
- reduce prompt token spend,
- improve reliability (less truncation and drift),
- keep everything MCP-compatible for agent tooling.

## 2) Market evidence (pricing + limits)
Use these numbers to anchor pricing and the ROI story:
- **OpenAI GPT-4.1**: 1M token context window; long‑context requests priced at standard per‑token rates. Pricing is $2.00 input / $0.50 cached input / $8.00 output per 1M tokens.
- **Anthropic Claude**: 1M context available to usage tier 4 (or custom limits); prompts >200k tokens are charged at premium rates (2x input, 1.5x output). Current pricing anchors: Claude Haiku 3.5 $0.80/MTok input, Sonnet 4 $3/MTok input, Opus 4 $15/MTok input.
- **Google Gemini**: Gemini 2.0 Flash models ship with a 1M token context window; Gemini 2.5 Pro pricing increases for prompts >200k tokens (input $1.25 → $2.50; output $10 → $15 per 1M tokens).
- **Vector DB anchors** (cloud pricing):
  - Pinecone Standard $50/mo minimum; Enterprise $500/mo minimum.
  - Weaviate Cloud entry tiers start at $45/mo; Plus starts at $280/mo.
  - Qdrant Cloud free tier includes a 1GB RAM / 4GB disk cluster.
- **Observability anchor**: Langfuse Cloud — Hobby free (50k units/month), Core $29/mo, Pro $199/mo; Enterprise via sales.

## 3) Positioning
**“Context OS for agents.”**
- Memory bank + retrieval + orchestration in front of any model.
- MCP hub makes it portable across IDEs and agent stacks.
- Converts long‑context costs into a predictable, cheaper memory bill.

## 4) Product tiers (proposed)
Open‑core with a managed SaaS and an enterprise tier.

### Self‑hosted (open‑core)
- Core MCP hub + memory bank + Qdrant integration.
- Community support.
- No hosted UI or multi‑tenant features.

### Managed SaaS (production)
- Multi‑tenant control plane.
- SSO, audit logs, rate limits, usage dashboards.
- SLA, backups, key rotation, retention policies.

### Enterprise
- BYOK, private networking, compliance artifacts, custom SLAs.
- Dedicated ingestion + isolation.

## 5) Pricing model (proposed)
**Hybrid seat + usage** (storage/reads/writes), anchored to competitor pricing.

| Tier | Target | Monthly (base) | Usage | Notes |
| --- | --- | --- | --- | --- |
| Starter | Individual / OSS | $0–$49 | Pay‑as‑you‑go | Self‑serve, limited retention |
| Team | Small teams | $199–$499 | Usage‑based | Shared memory + dashboards |
| Scale | Fast‑growing | $999+ | Usage‑based | Higher throughput + SLA |
| Enterprise | Regulated | $2,500+ | Contract | SSO/BYOK/Private networking |

Usage metric: “context units” (raw tokens processed + stored).
- This aligns with Langfuse’s usage unit model and Pinecone/Weaviate’s minimum spend expectations.
- Entry tier: allow routing to budget/legacy models (e.g., Gemini Flash, Claude Haiku, GPT‑4.1) or free‑tier endpoints for low‑risk tasks to reduce adoption friction.

## 6) Unit economics and ROI story
- **Savings claim**: “Cut prompt tokens by 30–80% with memory + compression.”
- **ROI math** (example):
  - If a team drops 1M input tokens/day on a premium model, monthly cost can be material; shifting to memory reduces this bill.
  - Long‑context pricing multipliers (Anthropic, Google) make memory cost‑avoidance easier to justify.

## 7) Launch phases
**Phase 0 – Internal readiness (now)**
- Harden auth, rate limits, usage metering, billing hooks.
- Define “context units” and storage metrics.
- Add org/project separation in memory bank + Qdrant namespaces.

**Phase 1 – Private beta (2–4 weeks)**
- 3–5 design partners; weekly usage reviews.
- Validate cost savings vs baseline prompts.
- Collect testimonials for landing page.

**Phase 2 – Public beta (4–8 weeks)**
- Self‑serve signup, docs, and a starter plan.
- Publish MCP integration guides and migration tools.

**Phase 3 – GA (8–12 weeks)**
- SLA and security docs.
- Launch paid plans + upgrade path.
- Sales pipeline for enterprise.

## 8) Production checklist
- **Infra**: autoscaling, regional routing, backups, retention.
- **Security**: TLS, least‑privilege RBAC, audit logs, secret rotation.
- **Reliability**: Qdrant snapshots, Mongo backup, HA for orchestration.
- **Compliance**: data residency toggle, DPA, privacy policy.
- **Billing**: Stripe + usage meter for “context units”.

## 9) Go‑to‑market
- **Positioning**: “Stop paying for long‑context tokens. Use memory.”
- **Channel**: IDE/MCP community, LLM ops teams, agents building on Claude/GPT/Gemini.
- **Proof**: before/after token usage and latency chart.

## 10) Risks + mitigations
- **Model pricing changes**: update pricing anchors quarterly.
- **Over‑promising savings**: provide conservative ROI estimates.
- **Data privacy concerns**: ship on‑prem/bring‑your‑own‑cloud option.

## Sources (for pricing anchors)
- OpenAI GPT‑4.1 release + OpenAI API pricing
- Anthropic context windows + pricing
- Gemini API pricing
- Pinecone pricing (minimums)
- Weaviate pricing
- Qdrant Cloud pricing/free tier
- Langfuse pricing
