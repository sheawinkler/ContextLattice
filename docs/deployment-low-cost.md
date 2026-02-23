# Low-Cost Deployment Playbook

The full ContextLattice stack can run on a single small VM, but several services (Langfuse, MindsDB, Promptfoo, Letta, Ollama) drive the memory, CPU, and storage bill. This guide explains how to launch a trimmed-down deployment for early adopters or demo tenants, and how to layer optional services back in when budget allows.

## 1. Choose the service tier

| Tier | Services | Notes |
| --- | --- | --- |
| **Core (essential)** | `qdrant`, `mongo`, `memorymcp-http`, `mcp-qdrant`, `mcp-hub`, `memmcp-orchestrator` | Powers the memory bank + semantic recall via HTTP-only MCP endpoints. Fits in <2 GB RAM.
| **LLM Support** | `ollama`, `letta` | Only if the host has spare CPU/RAM or a GPU. You may also point `trae` to an external LM Studio / OpenAI-compatible endpoint instead of running Ollama.
| **Analytics/SQL** | `mindsdb`, `mindsdb-http-proxy` | Optional enrichment (SQL-like joins over live data). Skip these when you just need memory + recall.
| **Observability** | `lf-postgres`, `lf-clickhouse`, `lf-redis`, `lf-minio`, `langfuse`, `langfuse-worker`, `promptfoo` | Heavy (databases + object storage). Use Langfuse Cloud or disable entirely for the cheapest footprint.

> Tip: You can start just the core tier with `docker compose up -d qdrant mongo memorymcp-http mcp-qdrant mcp-hub memmcp-orchestrator`.

## 2. Reference hardware sizing

| Footprint | CPU | RAM | Disk | Recommendation |
| --- | --- | --- | --- | --- |
| Core tier | 2 vCPU | 4 GB | 25 GB SSD | Handles Qdrant + Mongo + orchestrator comfortably.
| +LLM (Ollama 7B) | 4 vCPU | 16 GB | 60 GB SSD | Consider GPU-ready hosts if you want low-latency inference.
| +Observability | +2 vCPU | +8 GB | +100 GB SSD | ClickHouse loves fast NVMe; otherwise latency spikes.

## 3. Deployment steps (core tier)

1. **Provision host** – Ubuntu 22.04 VM (e.g., Hetzner CX22 / fly.io dedicated / Akamai Connected Cloud) with Docker + Docker Compose v2 installed.
2. **Sync repo + env** – copy this repo to the host and tailor `.env`:
   - Set `MONGODB_URI=mongodb://mongo:27017` (already default) and tighten credentials if running on a public cloud (use Docker networks + firewalls).
   - For Qdrant, keep `QDRANT_COLLECTION=memmcp_notes` or switch to a tenant-specific name.
   - If Langfuse Cloud is used later, set `LANGFUSE_URL` + `LANGFUSE_API_KEY` now (the orchestrator already respects them).
3. **Start only core services**:
   ```bash
   docker compose --profile core pull
   docker compose --profile core up -d --build
   docker compose ps
   ```
   Or via Make: `make up-core` (it simply forwards `PROFILES=core` to Compose).
4. **Smoke test**:
   ```bash
   curl -fsS http://localhost:6333/readyz            # Qdrant
   gmake mem-ping                                    # MCP hub → memorymcp tools/list
   curl -fsS http://127.0.0.1:8075/status | jq
   ```
5. **Point agents** – configure `trae_config.yaml` (or Letta) to hit the MCP hub at `http://<host>:53130/memorymcp/mcp` for memory tools. (Other servers: `/qdrant/mcp`, `/mindsdb/mcp`.)

## 4. Cost levers & alternatives

- **Observability:** Instead of self-hosting Langfuse + ClickHouse, set `LANGFUSE_URL` to the managed SaaS and only keep the API proxy locally. This drops ~8 GB RAM and ~60 GB disk.
- **Embeddings:** Set `ORCH_EMBED_PROVIDER` (`openai`, `lmstudio`, `ollama`, or `cheap`) plus `ORCH_EMBED_MODEL`/`EMBEDDING_BASE_URL`. The orchestrator now auto-creates the Qdrant collection using the returned vector dimension, so you can lean on a remote embedding API without hosting another pod.
- **LLM provider:** Use LM Studio or an OpenAI-compatible host elsewhere to save RAM locally. Update `trae_config.yaml` -> `clients.default.base_url` and leave `ollama` stopped unless needed for offline mode.
- **MindsDB-as-a-service:** MindsDB Cloud exposes HTTP + MySQL endpoints; you can point `mindsdb-http-proxy` at it by setting `MINDSDB_SSE_URL` to the hosted SSE gateway and skipping the local `mindsdb` container entirely.
- **Prompt evals:** For early launches skip `promptfoo` and rely on Langfuse (or structured JSON logs in Mongo). Re-enable when you build a QA program.

## 5. Compose profiles (now live)

Each service declares a Compose profile:

- `core`: qdrant, mongo, memorymcp-http, mcp-qdrant, mcp-hub, memmcp-orchestrator.
- `llm`: ollama + letta.
- `analytics`: mindsdb + mindsdb-http-proxy.
- `observability`: Langfuse stack + Promptfoo.

By default `COMPOSE_PROFILES=core,llm,analytics,observability` (see `.env`), so `docker compose up -d` still launches everything. To run the slim tier either export `COMPOSE_PROFILES=core` or pass profiles inline:

```bash
docker compose --profile core up -d --build
# or
PROFILES=core make up
```

Enable extra tiers by passing additional profiles (e.g., `--profile core --profile observability`) when you need Langfuse/Promptfoo.

## 6. Outstanding launch blockers

1. **Embedding alignment** – pick a real embedding backend and set `ORCH_EMBED_PROVIDER`/`EMBEDDING_BASE_URL`. The orchestrator now enforces matching vector sizes, but you still need to decide which hosted or local embedding model to fund.
2. **LLM backend** – Trae still needs a reliable inference endpoint (`lmstudio` w/ `/v1/chat/completions` shim or a lightweight hosted model). Without it, agents stall even if memory services are up.
3. **Dashboard bundling** – decide whether the Next.js dashboard (`memmcp-dashboard/`) should be part of the Compose stack or hosted separately (e.g., Vercel + API key). For low-cost launches it can be optional, but document how operators manage memory without it.
4. **Security pass** – lock down ports via firewall / reverse proxy (Caddy or Traefik) before exposing the MCP hub on the public internet. Free-tier Cloudflare Tunnel can front the stack cheaply.

Once these are addressed, we can push images to a registry and script a one-command bootstrap for new tenants.

## 7. Paid cloud quickstart

When you're ready to host ContextLattice for others:

1. Provision a small VM (e.g., Hetzner CX32 / AWS m7i.large) with Docker + Docker Compose installed.
2. Copy this repo + `.env` to the server and set `COMPOSE_PROFILES=core` for the initial bring-up.
3. `docker compose --profile core up -d` to launch the bare memory tier (or run `scripts/deploy_hosted_core.sh <domain> <email>` to both start the core stack and generate + launch a Caddy reverse proxy with automatic TLS).
4. Add `observability` and/or `analytics` profiles per customer tier: e.g., `docker compose --profile core --profile observability up -d` to include Langfuse/Promptfoo.
5. Point customer IDEs/agents at `https://<your-domain>/mcp` and manage tenant isolation via per-customer Memory Bank projects + Langfuse API keys.
6. Upsell hosted embeddings/LLMs by setting `ORCH_EMBED_PROVIDER=openai` and piping requests through your billed API keys.

Document these steps in customer onboarding materials and include support SLAs as part of the paid plan.
