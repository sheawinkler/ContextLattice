# Memory MCP Components & Interactions

## Overview
The ContextLattice stack is a collection of HTTP-first services that cooperate to provide a longitudinal memory layer for agents. Each component handles a single concern (storage, retrieval, observability, evaluation, orchestration) while communicating through standardized interfaces (MCP, REST, or HTTP streams). Interconnecting them unlocks persistent memory, searchable context, and structured observability—giving light LLMs superhuman recall.

## Component Breakdown

### Qdrant (Vector Store)
- **Role:** Stores embedding vectors for semantic search across prior conversations, notes, and trajectories.
- **Interface:** HTTP gRPC is disabled; we stick to HTTP REST via `http://qdrant:6333`.
- **Upstream Inputs:** The orchestrator pushes embeddings from memories/trajectories; agents query via MCP.
- **Downstream Consumers:** MCP Hub exposes Qdrant as a tool; Trae or Letta call `similarity_search` to recall context.

### Mongo (Raw Memory Bank)
- **Role:** Houses structured JSON/text files representing project notes, task histories, and user memories.
- **Interface:** Accessed indirectly through `memorymcp-http` which wraps the CLI-based memory-bank MCP.

### Memory Bank MCP (Supergateway HTTP)
- **Role:** Converts the stdio-based `@allpepper/memory-bank-mcp` into a proper HTTP/SSE MCP server at `http://localhost:59081/mcp`.
- **Capabilities:** `list_projects`, `list_project_files`, `memory_bank_read/write/update`.
- **Consumers:** Trae, Letta, Next.js dashboard, orchestrator.

### MindsDB + HTTP Proxy
- **Role:** Offers SQL-like access to live data sources and models via MCP. The FastMCP proxy exposes HTTP endpoints so agents can query MindsDB without SSE.
- **Interaction:** Trae/Letta can ask MindsDB for enriched signals (market data, etc.), then write summarized context into memory bank or Qdrant.

### MCP Hub (tbxark/mcp-proxy)
- **Role:** One-port gateway that routes MCP servers (Qdrant, Memory Bank, MindsDB) under `http://127.0.0.1:53130/<server>/mcp`.
- **Interaction:** Letta or any downstream agent registers a single HTTP MCP endpoint yet gains access to all configured tools.

### Langfuse (Observability)
- **Role:** Captures traces, evaluations, and metrics for every agent request. Uses Postgres + ClickHouse backend.
- **Interaction:** Orchestrator posts ingestion events (via Langfuse REST) whenever trajectories or memory writes occur, so humans can audit decisions.

### Promptfoo
- **Role:** Hosts evaluation dashboards/tests for prompts and chains. Useful for regression testing or demo dashboards.
- **Interaction:** Notified by orchestrator after new trajectories land, enabling one-click eval re-runs.

### Letta (Agent Runner)
- **Role:** Alternative to Trae for long-running agentic workflows, already configured to use Ollama and MCP hub.

### New: ContextLattice Orchestrator (HTTP glue)
- **Role:** FastAPI service (`http://127.0.0.1:8075`) that:
  - Accepts trajectory payloads (e.g., POST `/ingest/trajectory`).
  - Writes summaries to the memory bank via MCP HTTP.
  - Pushes semantic embeddings to Qdrant.
  - Emits Langfuse ingestion events for observability.
  - Provides an optional agent task queue at `/agents/tasks`.
  - Stores feedback + preference context and injects it into retrieval by default.
  - Enforces high-risk action approvals for queued tasks.
  - Maintains topic trees for scoped retrieval by topic/subtopic.
- **Interaction:** Trae/Next.js can POST to the orchestrator instead of juggling multiple backends.

### New: Next.js Dashboard (`memmcp-dashboard/`)
- **Role:** Operator UI for memory bank. Lists projects/files, previews content, and appends new notes via HTTP calls to the orchestrator.
- **Interaction:** Uses Next API routes to proxy MCP calls, ensuring browsers never need direct MCP headers.

### Strategy Override Feed
- **Role:** Surfaces the Sol-scaling bot's override events (size/confidence boosts) that are logged to the `sol_scaler_overrides` project in ContextLattice.
- **Orchestrator:** Background task polls `memory_bank_read` for new override files and serves them via `GET /overrides/latest` with a pluggable history limit.
- **Dashboard:** `OverridesPanel` consumes `/api/overrides/latest` and renders the latest boosts (priority, reason, pre/post size, confidence deltas). This keeps ops aware when Trae/Xavier enlarge trades.
- **Env knobs:**
  - `OVERRIDE_PROJECT`, `OVERRIDE_HISTORY_LIMIT`, `OVERRIDE_FETCH_LIMIT`, `OVERRIDE_REFRESH_SECONDS` – orchestrator-side tuning for retention/poll cadence.
  - `MEMMCP_OVERRIDE_PROJECT` – trading bot env var that controls where override events are logged (default `sol_scaler_overrides`).

## How It All Fits Together
1. **Agent writes memory:** Trae calls the memory MCP to log notes; orchestrator simultaneously mirrors the entry into Qdrant (embedding) and Langfuse (trace).
2. **Agent recalls context:** Through MCP Hub, the agent asks Qdrant for semantic neighbors—instantly fetching long-term context without re-processing entire history.
3. **Observability:** Langfuse captures every trace, evaluation, and ingestion so humans can monitor bias, drift, or hallucinations.
4. **Evaluation loop:** Promptfoo pulls traces (via orchestrator hooks) and replays evals, giving a tight CI/CD loop for prompts.
5. **Dashboard:** Operators inspect/curate memory entries through the Next.js UI, optionally editing notes that feed back into Qdrant + Langfuse.

The result is a hyper-efficient memory layer: every piece of context becomes searchable (Qdrant), inspectable (dashboard), auditable (Langfuse), and actionable (MindsDB analytics). Because everything speaks HTTP, plugging new MCP clients or hosted offerings is trivial.

## Cross-Project Guardrails
- **Namespace everything:** Each payload the Solana trading bot emits is tagged with `project=sol_scaler` plus fine-grained `kind` labels (e.g., `telemetry`, `strategy_metrics`). Other projects can safely ignore or filter these tags, keeping the core memory corpus clean.
- **Keep trading logic out of this repo:** The bot remains in `~/Documents/Projects/algotraderv2_rust`, and any future trading-only modules live there (or in sibling repos). ContextLattice only exposes HTTP endpoints and dashboards that *consume* those events—if a component starts to require trading-specific business logic, we spin it out of this codebase.
- **Document shared patterns:** Notes like `docs/xavier_integration.md` exist solely to explain how an external client can leverage ContextLattice; they should never hard-code trading assumptions into the orchestrator or dashboard.
- **Monitor growth:** Periodically review stored collections (Qdrant, Mongo) so trading telemetry does not drown global knowledge. If a namespace balloons, export/archive it or graduate that workload to a dedicated storage service.
  - Run `scripts/storage_audit.py` (optionally with `--json`) to capture point/doc counts and check them into `reports/storage/` for historical diffing.

## Monetization + Vendor Notes
- **Paid support first:** ship the stack as a downloadable bundle and charge for installation/support. Infrastructure costs stay near-zero while we validate demand.
- **Cloud path:** hosting just requires Docker + an HTTPS ingress; we can start with a single VM per customer and scale horizontally later.
- **3rd-party licenses:**
  - *Langfuse* (AGPL-like license) – for commercial SaaS we can either purchase Langfuse Cloud or replace it with OpenTelemetry + Grafana if licensing becomes restrictive.
  - *Promptfoo* (OSS) – can be swapped with a lightweight Next.js eval board or even our new dashboard if we need a simpler UI.

Log this note in the memory bank via the orchestrator so planning discussions remain searchable.
