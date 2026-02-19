# Audit log operations

## Export
From `memmcp-dashboard/`:

```
npm run audit:export
```

Configure with:
- `AUDIT_EXPORT_DIR`
- `AUDIT_EXPORT_LIMIT`

## Retention
Prune logs older than the retention window:

```
npm run audit:prune
```

Configure with:
- `AUDIT_RETENTION_DAYS`
