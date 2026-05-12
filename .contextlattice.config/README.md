# .contextlattice.config

Canonical repo-local runtime configuration for ContextLattice helper surfaces.

## Purpose
- Keep local runtime glue config separated from product config under `config/`.
- Avoid product-name-specific legacy path assumptions.

## Layout
- `mcp-hub/config.json` — streamable MCP hub client targets.

## Compatibility
- Optional local shim path `.mcp-servers/contextlattice` can point to this
  directory for developer-specific tooling.
- New automation should read/write `.contextlattice.config/mcp-hub/config.json`
  directly.
