# Usage budgets

Budgets help prevent runaway spend. When enabled, requests beyond the budget return `402`.

## Enable enforcement
Budget enforcement is on by default. Set `ENFORCE_BUDGETS=false` in `.env` or `memmcp-dashboard/.env` to allow usage overages.

## Set a budget
`POST /api/workspace/budget`

```json
{ "tokenLimit": 200000, "costLimitUsd": 50 }
```

## Inspect budget + usage
`GET /api/workspace/budget`

Returns the active budget and month-to-date usage totals.

## Record usage events
`POST /api/usage/events`

```json
{
  "tokens": 1234,
  "costUsd": 0.42,
  "model": "gpt-4.1",
  "source": "pipeline.run",
  "metadata": { "project": "demo" }
}
```

## Anomaly thresholds
Set alert thresholds to write audit events when single events exceed limits:

- `USAGE_ANOMALY_TOKEN_THRESHOLD`
- `USAGE_ANOMALY_COST_THRESHOLD`

## Hard stop behavior
- The usage events endpoint checks the incoming event tokens/cost against the remaining budget.
- The memory write endpoint estimates tokens from the payload and blocks writes that would exceed the budget.
