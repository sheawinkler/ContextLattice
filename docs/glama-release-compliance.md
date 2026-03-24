# Glama Release Compliance

This guide defines the exact settings for Glama release configuration.

## Dockerfile release settings

Use these in the Glama Dockerfile admin form:

- Dockerfile path: `Dockerfile.orchestrator`
- Build context: `.`
- Command: use Dockerfile default command
- Port: `8075`
- Health endpoint: `/health`
- Build steps: `[]` (leave empty when Dockerfile path is set)
- CMD arguments: `[]` (use Dockerfile default command)

These are deploy settings, not environment variables.

## Arguments JSON schema

Use the schema box for runtime variables only.

Use `CONTEXTLATTICE_*` names in Glama.

- `CONTEXTLATTICE_ENV`: `development` (default) or `production`
- `CONTEXTLATTICE_ORCHESTRATOR_API_KEY`: required only when `CONTEXTLATTICE_ENV=production`
- `SECRETS_STORAGE_MODE`: `redact` (default), `block`, or `allow`
- `MESSAGING_OPENCLAW_STRICT_SECURITY`: optional boolean string
- `IRONCLAW_INTEGRATION_ENABLED`: optional boolean string
- `IRONCLAW_DEFAULT_PROJECT`: optional string

Legacy `MEMMCP_*` names are no longer the canonical public configuration.

## Python Version Compatibility

Release `v3.2.9` updates orchestrator dependency pins for CPython 3.14 build images:

- `pydantic==2.12.5`
- `orjson==3.11.7`

## Timing for score updates

- Repo metadata checks (README/license/glama.json): usually 10-60 minutes, sometimes longer.
- Quality/security checks: update after successful Glama Deploy + Release.
