# Contributing

Thanks for contributing to memMCP! This repo focuses on reliable memory, MCP routing, and orchestration across agents. The fastest way to help is to keep changes small, testable, and well‑documented.

## Quick start
1. Fork + clone.
2. Copy env: `cp .env.example .env` (adjust if needed).
3. Bring up core services:
   ```bash
   docker compose --profile core up -d --build
   ```
4. Verify the stack:
   ```bash
   curl -fsS http://127.0.0.1:8075/status | jq
   gmake mem-ping
   ```

## Local development
- **Core runtime**: Docker Compose (`docker-compose.yml`).
- **Profiles**: `core`, `analytics`, `observability`, `llm`.
- **Logs**: `docker compose logs -f <service>`.

## Testing & checks
- `gmake env-check` (compose validation)
- `gmake mem-ping` (MCP hub tools/list)
- `curl http://127.0.0.1:8075/status` (orchestrator health)

If you add new services or endpoints, include a short smoke test in the PR body.

## Pull requests
- Keep PRs focused (ideally <300 lines of diff).
- Update docs if you change endpoints, env vars, or runbooks.
- Use clear titles: `fix: ...`, `chore: ...`, `docs: ...`.

## Reporting bugs
Open an issue with:
- Expected vs actual behavior
- Repro steps
- Logs (minimal, redacted)
- Environment (OS, Docker, branch/commit)

## Security
See `SECURITY.md` for vulnerability reporting.
