# API keys (ContextLattice)

## Overview
API keys are workspace-scoped. Keys are returned once on creation and stored hashed.

## Create key
`POST /api/workspace/api-keys`

```json
{ "name": "My key", "scopes": "memory:write,usage:write" }
```

Response includes `apiKey` once.

## List keys
`GET /api/workspace/api-keys`

Returns key metadata (prefix, created, last used). Secrets are never returned again.

## Revoke key
`DELETE /api/workspace/api-keys/{id}`

## Using a key
Pass `X-API-Key` or `Authorization: Bearer <key>` to:
- `/api/memory/write`
- `/api/usage/events`

API keys are intended for production integrations; enable budgets to enforce spend caps.

### Scopes
Currently supported scopes:
- `memory:write`
- `usage:write`
