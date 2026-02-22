# Submission Requirements Audit

Last verified: 2026-02-21

This maps current submission-site requirements to repository collateral so launch ops can execute quickly.

## Requirement Matrix

| Site | Requirement snapshot | Repo coverage | Status |
| --- | --- | --- | --- |
| MCP Registry (official) | Registry expects a schema-valid server manifest (`name`, description, status, transport/package metadata) and publish via official registry tooling. | `registry/contextlattice.server.template.json`, `registry/README.md`, launch copy + docs URLs exist. | Ready, pending final endpoint |
| Glama MCP | Listing plus optional claim verification via `/.well-known/glama.json` email ownership proof. | `docs/public_overview/.well-known/glama.json`, `docs/public_overview/.nojekyll`, public docs + repo URL. | Ready |
| PulseMCP | Submission page routes through official MCP registry ingest; weekly fallback email if missed. | MCP registry template + docs/repo metadata prepared. | Ready after MCP registry publish |
| MCP.so | Submit form expects title/description/server URL/server config + links. | `docs/launch_channel_copybook.md`, `launch_service/config/contextlattice.launch.json`, README/docs links prepared. | Ready |
| Product Hunt | Launch checklist expects clear one-liner/tagline, media assets, links, maker FAQ response plan. | `README.md`, `docs/launch_channel_copybook.md`, `docs/public_overview/assets/contextlattice-og-1200x630.png`, `docs/public_overview/assets/contextlattice-icon-512.png`. | Ready |
| FutureTools | Form expects name, URL, category, description, pricing, contact details, optional image. | Launch copybook fields + public docs + contact email on `docs/public_overview/contact.html`. | Ready |
| Futurepedia | Submission flow with review/priority tiers; needs clear product metadata and links. | Launch copybook + tracker + public overview/docs links. | Ready |
| Toolify | Form expects tool name, URL, category, price, short/long descriptions, logo, contact email. | Launch copybook + launch config + public icon asset + contact page. | Ready |

## Automated Preflight

Run the repository preflight checker before submissions:

```bash
gmake submission-preflight
python3 scripts/submission_preflight.py --online
```

The checker verifies:
- required docs/legal/community files
- launch config channel metadata
- public listing assets + OpenGraph metadata
- Glama claim file validity
- optional online endpoint reachability

## Manual gates that remain

1. Publish final MCP Registry record using a real public MCP endpoint in `registry/contextlattice.server.json`.
2. Complete account-gated submissions (Product Hunt, FutureTools/Futurepedia/Toolify forms).
3. Capture submission IDs and update `docs/publish_execution_tracker.md` statuses from `Scheduled` to `Submitted/Live`.

## Sources

- MCP Registry quickstart: https://modelcontextprotocol.io/registry/quickstart
- MCP Registry package managers: https://modelcontextprotocol.io/registry/package-managers
- MCP Registry schema notes: https://modelcontextprotocol.io/registry/schema
- Glama server listing: https://glama.ai/mcp/servers
- Glama claim FAQ: https://glama.ai/mcp/connectors/ai/safe-integration
- PulseMCP submit: https://www.pulsemcp.com/submit
- MCP.so submit: https://mcp.so/submit
- Product Hunt launch: https://www.producthunt.com/launch
- FutureTools submit: https://www.futuretools.io/submit-a-tool
- Futurepedia submit: https://www.futurepedia.io/submit-tool
- Toolify submit: https://www.toolify.ai/submit
