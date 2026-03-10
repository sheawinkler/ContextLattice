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

Run locally:

```bash
cd services/gateway-go
go run .
```

Default port: `8091`
