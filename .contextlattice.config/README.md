# .contextlattice.config

Canonical repo-local runtime configuration for ContextLattice helper surfaces.

## Purpose
- Keep local runtime glue config separated from product config under `config/`.
- Avoid product-name-specific legacy path assumptions.
- Preserve compatibility with older tooling expecting `.mcp-servers/mem_mcp_lobehub`.

## Layout
- `mcp-hub/config.json` — streamable MCP hub client targets.

## Compatibility
- Legacy path `.mcp-servers/mem_mcp_lobehub` is maintained as a symlink to
  `.contextlattice.config/mcp-hub`.
- New automation should read/write `.contextlattice.config/mcp-hub/config.json`
  directly.
