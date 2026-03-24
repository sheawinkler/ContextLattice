# Glama Release Compliance

This guide defines the exact settings for Glama release configuration.

## Dockerfile release settings

Use these in the Glama Dockerfile admin form:

- Dockerfile path: `Dockerfile.orchestrator`
- Build context: `.`
- Command: use Dockerfile default command
- Port: `8075`
- Health endpoint: `/health`

These are deploy settings, not environment variables.

## Arguments JSON schema

Use the schema box for runtime variables only.

Recommended required variable:

- `MEMMCP_ORCHESTRATOR_API_KEY` (or `CONTEXTLATTICE_ORCHESTRATOR_API_KEY`)

Optional variables:

- `MEMMCP_ENV` (`development` or `production`)
- `HOST_BIND_ADDRESS`
- `SECRETS_STORAGE_MODE`

If the server runs in development mode, an API key placeholder can be optional.
If production mode is used, API key should be required.

## Timing for score updates

- Repo metadata checks (README/license/glama.json): usually 10-60 minutes, sometimes longer.
- Quality/security checks: update after successful Glama Deploy + Release.
