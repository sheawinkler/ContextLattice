# orchestrator-go

Go scheduler service for task queue orchestration.

Scope:
- `/v1/tasks/*` queue lifecycle endpoints.
- Queue metrics and health endpoints.

Note:
- Retrieval and memory engine APIs are served through `gateway-go` and backend engine service.

Run locally:

```bash
cd services/orchestrator-go
go run .
```

Default port: `8090`
