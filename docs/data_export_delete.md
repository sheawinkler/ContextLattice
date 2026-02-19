# Data export + deletion

## Export
`GET /api/workspace/export`

Requires workspace owner. Returns a JSON bundle of workspace metadata, API keys
metadata, budgets, usage events, and audit logs. Memory bank files are exported
separately from the memorymcp store.

## Deletion request
`POST /api/workspace/delete`

Marks the workspace as `pending_delete`, revokes API keys, and disables budgets.
Actual memory bank data must be purged from the memorymcp store before final
removal.
