# gateway-go

Go ingress gateway for ContextLattice runtime service-mode APIs.

Responsibilities:
- Provide a stable Go front-door for retrieval + memory engine APIs.
- Proxy `/v1/retrieval/*` and `/v1/memory/*` calls to the backend engine URL.
- Keep Python as backend fallback while preserving a Go-first network path.

Env:
- `PORT` (default `8091`)
- `BACKEND_URL` (default `http://contextlattice-orchestrator:8075`)
- `GATEWAY_PROXY_TIMEOUT_SECS` (default `95`)
- `CONTEXTLATTICE_ORCHESTRATOR_API_KEY` (optional injected key for `/tools/*` calls)
- `CONTEXTLATTICE_WORKER_API_KEY` (optional worker key for role-split tool policy)
- `GO_TOOL_CALLS_ALLOW_ALL` (default `true`)
- `GO_TOOL_CALLS_ROLE_SPLIT_AUTO` (default `true`, only activates with distinct orchestrator/worker keys)
- `GO_TOOL_CALLS_ROLE_SPLIT_ENABLED` (manual override, default `false`)

Run locally:

```bash
cd services/gateway-go
go run .
```

Default port: `8091`
