# gateway-go

Go ingress gateway for ContextLattice runtime service-mode APIs.

Responsibilities:
- Provide a stable Go front-door for retrieval + memory engine APIs.
- Proxy `/v1/retrieval/*` and `/v1/memory/*` calls to the backend engine URL.
- Keep Python as backend fallback while preserving a Go-first network path.
- Serve Go-native storage governance ops endpoints:
  - `GET /telemetry/storage`
  - `GET /telemetry/storage/ledger`
  - `POST /maintenance/storage/run`
  - `POST /maintenance/telemetry/blob-gc`

Env:
- `PORT` (default `8091`)
- `BACKEND_URL` (default `http://contextlattice-orchestrator:8075`)
- `GATEWAY_PROXY_TIMEOUT_SECS` (default `95`)
- `CONTEXTLATTICE_ORCHESTRATOR_API_KEY` (optional injected key for `/tools/*` calls)
- `CONTEXTLATTICE_WORKER_API_KEY` (optional worker key for role-split tool policy)
- `GO_TOOL_CALLS_ALLOW_ALL` (default `true`)
- `GO_TOOL_CALLS_ROLE_SPLIT_AUTO` (default `true`, only activates with distinct orchestrator/worker keys)
- `GO_TOOL_CALLS_ROLE_SPLIT_ENABLED` (manual override, default `false`)
- `GO_RETRIEVAL_SYNC_SOURCE_CONCURRENCY_DEFAULT` (default `2`)
- `GO_RETRIEVAL_SYNC_SOURCE_CONCURRENCY_OVERRIDES` (JSON object by source lane)
- `GO_RETRIEVAL_SYNC_QUEUE_AGE_WARN_SECS` (default `2.0`)
- `GO_RETRIEVAL_SYNC_QUEUE_AGE_HIGH_SECS` (default `5.0`)
- `GO_RETRIEVAL_CONTINUATION_SHEDDING_ENABLED` (default `true`)
- `GO_RETRIEVAL_CONTINUATION_SHEDDING_QUEUE_RATIO` (default `0.85`)
- `GO_RETRIEVAL_CONTINUATION_SHEDDING_PENDING_HIGH` (default `max(2, continuation_max_inflight-1)`)
- `GO_RETRIEVAL_CONTINUATION_SHEDDING_SOURCES` (default `letta,memory_bank,mongo_raw,mindsdb`)
- `ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_SOURCES` (default `letta,memory_bank,mindsdb,mongo_raw,qdrant`)
- `GO_RETRIEVAL_CONTINUATION_DURABLE_MAX_PENDING_PER_SOURCE` (default `24`)
- `GO_WRITE_DEFAULT_AGENT_ID`, `GO_WRITE_DEFAULT_SESSION_ID`, `GO_WRITE_DEFAULT_TAGS` (fallback metadata for writes that omit canonical fields)
- `GO_RETRIEVAL_TIMEOUT_CONTRACT_GRACE_SECS` (default `0.075`)
- `ORCH_STORAGE_LEDGER_PATH` (optional explicit path; default resolves from `GO_MEMORY_STORE_ROOT/_contextlattice/storage_ledger.ndjson`)
- `ORCH_STORAGE_LEDGER_READ_LIMIT_DEFAULT` (default `168`)
- `ORCH_STORAGE_LEDGER_READ_LIMIT_MAX` (default `5000`)

Run locally:

```bash
cd services/gateway-go
go run .
```

Default port: `8091`
