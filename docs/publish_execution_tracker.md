# ContextLattice Publish Execution Tracker (MCP Service)

Last updated: 2026-02-22
Launch window: Catch-up execution wave: Monday, February 23, 2026

## 1) Positioning Guardrails

- Classify as `MCP server` / `MCP service` / `local-first memory orchestration service`.
- Do not use plugin/template-style labeling in public listings.
- Category anchor: `MCP Server / AI Infrastructure`.

## 2) Canonical Launch Metadata

| Field | Value |
| --- | --- |
| Product name | ContextLattice |
| Category | MCP Server / AI Infrastructure |
| Primary URL | `https://contextlattice.io/` |
| Docs URL | `https://contextlattice.io/installation.html` |
| Troubleshooting URL | `https://contextlattice.io/troubleshooting.html` |
| Repo URL | `https://github.com/sheawinkler/ContextLattice` |
| Public overview URL | `https://contextlattice.io/` |
| One-liner | Private-by-default long-horizon memory, context, and task orchestrator for every LLM, everywhere. |
| Primary CTA | Start local-first in minutes with `gmake quickstart`. |

## 3) Channel Tracker

| Tier | Channel | Listing URL | Submission path | Lead time | Cost signal | Owner | Scheduled publish (MT / PT) | Status |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| P0 | GitHub Release | https://github.com/sheawinkler/ContextLattice/releases/new | Tag v1.0.0 + release notes + checksums + images | Same day | Free | Shea | 2026-02-23 00:00 / 2026-02-22 23:00 | Live |
| P0 | Custom Domain Docs | https://contextlattice.io | DNS + CNAME + HTTPS + smoke tests | 1-2 days | Domain/DNS | Shea | 2026-02-23 00:00 / 2026-02-22 23:00 | Live |
| P0 | MCP Registry (official) | https://registry.modelcontextprotocol.io | Publish server metadata via registry tooling | 1-3 days | Free | Shea | 2026-02-20 15:30 / 2026-02-20 14:30 | Ready (manual submit) |
| P0 | Glama MCP | https://glama.ai/mcp/servers | Add server listing + docs + repo links | 0-2 days | Free | Shea | 2026-02-20 16:00 / 2026-02-20 15:00 | Ready (manual submit) |
| P0 | PulseMCP | https://www.pulsemcp.com/submit | Submit MCP server profile + launch links | 1-7 days | Free | Shea | 2026-02-20 16:20 / 2026-02-20 15:20 | Ready (manual submit) |
| P0 | MCP.so | https://mcp.so/submit | Submit listing card with MCP service metadata | 0-3 days | Free | Shea | 2026-02-20 16:40 / 2026-02-20 15:40 | Ready (manual submit) |
| P0 | Product Hunt | https://www.producthunt.com/launch | Launch page + comments + maker FAQ replies | 1-3 days prep | Free | Shea | 2026-02-21 08:05 / 2026-02-21 07:05 | Ready (manual submit) |
| P0 | Show HN | https://news.ycombinator.com/showhn.html | Post Show HN with runnable local install | Same day | Free | Shea | 2026-02-21 08:10 / 2026-02-21 07:10 | Ready (manual submit) |
| P0 | X (launch thread) | https://x.com | 1 launch post + 1 technical follow-up thread | Same day | Free | Shea | 2026-02-21 08:20 / 2026-02-21 07:20 | Ready (manual submit) |
| P0 | LinkedIn | https://www.linkedin.com | Founder launch post + architecture image | Same day | Free | Shea | 2026-02-21 08:25 / 2026-02-21 07:25 | Ready (manual submit) |
| P0 | Reddit (targeted) | https://www.reddit.com | Subreddit-specific posts with value-first context | Same day | Free | Shea | 2026-02-21 08:35 / 2026-02-21 07:35 | Ready (manual submit) |
| P0 | Hugging Face Release (Spaces) | https://huggingface.co/spaces | Publish launch Space + README card + repo/docs links | 1-2 days | Free | Shea | 2026-02-21 08:45 / 2026-02-21 07:45 | Ready (manual submit) |
| P1 | FutureTools | https://www.futuretools.io/submit-a-tool | Manual tool submission flow | 1-7 days | Free | Shea | 2026-02-20 17:00 / 2026-02-20 16:00 | Ready (manual submit) |
| P1 | Futurepedia | https://www.futurepedia.io/submit-tool | Submission flow (paid options may apply) | 1-7 days | Paid optional | Shea | 2026-02-20 17:20 / 2026-02-20 16:20 | Ready (manual submit) |
| P1 | Toolify | https://www.toolify.ai/submit | Submission flow (paid options may apply) | 1-3 days | Paid optional | Shea | 2026-02-20 17:40 / 2026-02-20 16:40 | Ready (manual submit) |
| P1 | Dev.to Deep-dive | https://dev.to | Technical launch article with install proof | Same day | Free | Shea | 2026-02-21 09:30 / 2026-02-21 08:30 | Ready (manual submit) |

## 4) Launch-Day Run of Show

1. `08:30 MT / 07:30 PT` - Launch day preflight on Saturday, February 21, 2026: stack (`gmake mem-up-full`), check `/health` and `/status`, verify release links.
2. `08:50 MT / 07:50 PT` - Freeze launch copy and schedule drafts.
3. `09:00 MT / 08:00 PT` - Publish GitHub release and custom-domain docs.
4. `09:05 MT / 08:05 PT` - Publish Product Hunt listing.
5. `09:10 MT / 08:10 PT` - Publish Show HN post.
6. `09:20 MT / 08:20 PT` - Publish X launch thread.
7. `09:25 MT / 08:25 PT` - Publish LinkedIn post.
8. `09:35 MT / 08:35 PT` - Publish Reddit posts.
9. `09:45 MT / 08:45 PT` - Publish Hugging Face Space release.
10. `10:30 MT / 09:30 PT` - Publish technical deep-dive article and respond to launch threads.

## 5) KPI Targets

| KPI | 24h target | 7d target | Source |
| --- | --- | --- | --- |
| GitHub stars | 100+ | 400+ | GitHub |
| Docs unique visitors | 1,500+ | 8,000+ | Web analytics |
| Quickstart completions | 50+ | 250+ | Issues/discussions + telemetry |
| MCP listings live | 4+ | 8+ | Manual tracker |
| Demo watch-through | 30%+ | 35%+ | Video host analytics |

## 6) Risk Register

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Listing review lag | Asynchronous launch windows | Submit MCP directories by 2026-02-20 and track confirmations |
| Terminology drift | User confusion in discovery channels | Use MCP server/service language in every listing and copy block |
| Launch-day service hiccups | Trust loss | Warm stack 30 minutes early and pin troubleshooting link |
| Paid board underperformance | Low ROI | Cap spend and prioritize MCP-native channels first |

## 7) Source Links

- Product Hunt Launch Guide: `https://www.producthunt.com/launch`
- Show HN Guidelines: `https://news.ycombinator.com/showhn.html`
- MCP Registry quickstart: `https://modelcontextprotocol.io/registry/quickstart`
- MCP Registry FAQ: `https://modelcontextprotocol.io/registry/faq`
- Glama MCP Servers: `https://glama.ai/mcp/servers`
- PulseMCP submit: `https://www.pulsemcp.com/submit`
- MCP.so submit: `https://mcp.so/submit`
- Hugging Face Spaces: `https://huggingface.co/spaces`
- FutureTools submit: `https://www.futuretools.io/submit-a-tool`
- Futurepedia submit: `https://www.futurepedia.io/submit-tool`
- Toolify submit: `https://www.toolify.ai/submit`

