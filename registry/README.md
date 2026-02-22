# MCP Registry Submission Files

This folder holds MCP Registry submission collateral.

## Files

- `contextlattice.server.template.json` - ready-to-edit template aligned with MCP Registry schema.

## How to use

1. Copy template and set a real public MCP endpoint:

```bash
cp registry/contextlattice.server.template.json registry/contextlattice.server.json
```

2. Update `remotes[0].url` and any auth header strategy.
3. Validate and publish with official tooling:

```bash
npx @modelcontextprotocol/registry validate registry/contextlattice.server.json
npx @modelcontextprotocol/registry publish registry/contextlattice.server.json
```

Notes:
- Server names should stay namespaced (`io.github.<owner>/<server>`).
- PulseMCP ingestion is tied to official registry publication.
