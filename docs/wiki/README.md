# ContextLattice Wiki (Public/Main)

This is the repository-side mirror of the public wiki experience.

- Live site: <https://contextlattice.io/wiki.html>
- Source page: `docs/public_overview/wiki.html`
- Visual atlas asset: `docs/public_overview/assets/wiki-tool-atlas.svg`

## What this wiki covers

- Endpoint atlas (`/memory/write`, `/memory/search`, `/memory/context-pack`, `/v1/memory/neighbors`, continuation events)
- Retrieval mode ladder (`fast`, `balanced`, `deep`) with practical timeout guidance
- Staged-fetch and async continuation behavior
- Production playbooks (launch, retrieval-quality, incident-response, release)
- Agent integration template locations

## Start here

1. Open the live wiki: <https://contextlattice.io/wiki.html>
2. Verify local health: `GET /health` and authenticated `GET /status`
3. Run one write and one search smoke call
4. Use scoped `fast` retrieval first, then escalate to `balanced`/`deep` when needed

## Related docs

- `README.md`
- `docs/public_overview/installation.html`
- `docs/public_overview/integration.html`
- `docs/public_overview/troubleshooting.html`
- `docs/public_overview/architecture.html`
