# kalliste-alpha — Roadmap (Implementation Steps)

This roadmap is **agent- and dev-friendly**: each section has concrete tasks you can execute. We standardize on HTTP-only MCP (no stdio/SSE), POSTing JSON-RPC to the /mcp/ endpoint with headers:

- Accept: application/json, text/event-stream
- Content-Type: application/json
- MCP-Protocol-Version: 2025-11-25

Primary demo endpoint: http://127.0.0.1:53130/memorymcp/mcp (MCP hub → memory bank).  
Supergateway exposes stdio servers via Streamable HTTP at /mcp.

---

## Repo Structure (suggested)

The following structure is a recommendation; adapt as needed.

    docs/                    # README, pricing, roadmap, architecture notes
    config/                  # pricing_calculator.json, runtime config templates
    infra/
      compose/               # compose overrides, lock files, healthchecks
      k8s/                   # (later) Helm chart / manifests
    server/
      http/                  # HTTP (MCP) entrypoints, middleware (auth/quota/logging)
      core/                  # memory domain logic
      metrics/               # Prometheus metrics
      jobs/                  # background jobs (rollups, retention)
    scripts/                 # local tooling (lint, snapshot, seed)

---

## A) Metering & Quotas (usage billing backbone)

**Goal:** Meter every HTTP JSON-RPC call and enforce plan caps.

**Implementation tasks (agent-friendly):**
- [ ] Create server/http/middleware/meter.ts
  - Capture for each request: { ts, project_id, org_id, method, bytes_in, bytes_out, status }.
  - Append to a local write-ahead log (logs/usage.jsonl) and emit to a queue (Kafka/SQS) when configured.
- [ ] Create jobs/usage/rollup.ts
  - Hourly: aggregate to daily_usage(project_id, calls, bytes_in/out).
  - Materialize org/project daily rows in a lightweight store (Postgres/SQLite).
- [ ] Create server/http/middleware/quota.ts
  - Read plan limits from entitlement cache.
  - Check rolling counters (e.g., 30-day window) before dispatch.
  - On exceed: return a JSON-RPC error with code -32000 and message "quota_exceeded".
- [ ] Storage metering: jobs/storage/sizes.ts
  - Nightly compute GB per project (filesystem/DB stats) and store monthly averages for billing.

**Suggested metrics (Prometheus names):**
- mcp_http_requests_total{method,status}
- mcp_http_request_bytes_total
- mcp_http_response_bytes_total
- mcp_http_duration_seconds (histogram)
- quota_block_total{reason}

---

## B) AuthN/AuthZ (Free → Pro → Team)

**Goal:** OAuth/OIDC for Pro; add SAML/OIDC enterprise SSO + RBAC.

**Implementation tasks:**
- [ ] server/http/auth/oidc.ts — GitHub/Google for Pro; generic OIDC for Team/Enterprise.
- [ ] server/http/auth/saml.ts — SAML SP metadata + ACS endpoint for enterprise.
- [ ] server/http/auth/api_keys.ts — create/rotate/revoke project API keys.
- [ ] RBAC model: owner | admin | member | viewer; enforce in middleware.
- [ ] Session storage: signed cookies or short-lived JWTs with refresh token flow.

---

## C) Billing (Stripe) & Plans

**Goal:** Seat + org + usage overages.

**Implementation tasks:**
- [ ] Create Stripe catalog entries:
  - Products: kalliste-pro-seat, kalliste-team-seat, kalliste-team-org.
  - Meters: jsonrpc_calls, storage_gb_month.
- [ ] server/http/billing/stripe_webhooks.ts
  - Handle checkout.session.completed, customer.subscription.updated, invoice.paid, usage reports.
  - On payment success: update entitlements cache (entitlements/{org_id}.json).
- [ ] jobs/usage/stripe_report.ts
  - Push daily/weekly rollups to Stripe usage meters.
- [ ] Dunning flow: implement grace period → downgrade path on unpaid invoices.

---

## D) Enterprise Features

**Goal:** Fulfill enterprise requirements (security, HA, compliance).

**Implementation tasks:**
- [ ] server/http/admin/backup.ts and restore.ts with S3/GCS adapters.
- [ ] jobs/retention/purge.ts — implement retention policies per org/project.
- [ ] server/http/admin/audit_export.ts — export audit logs as JSONL.
- [ ] infra/k8s/ — Helm chart with values for VPC, private networking.
- [ ] infra/compose/.compose.lock.yml — reproducible on-prem compose snapshot for PoC.
- [ ] SLA runbooks: on-call, RTO/RPO, incident response templates.

---

## E) Docs & DX (MCP-native)

**Goal:** 90-second integration flow and quality developer experience.

**Implementation tasks:**
- [ ] docs/README.mcp-http.md — quickstart (curl, Node, Python) showing HTTP-only POST to /mcp/ with required headers.
- [ ] docs/snippets/node/fetch_initialize.js — minimal client example.
- [ ] docs/snippets/python/requests_tools_list.py — minimal client example.
- [ ] Provide a one-line deploy: docker compose -f infra/compose/.compose.lock.yml up -d.

---

## F) Security & Limits

**Goal:** Practical controls for early GA.

**Implementation tasks:**
- [ ] Project-scoped tokens with creation metadata and last-used timestamps.
- [ ] Rate-limits (token + IP): e.g., 100 RPS burst, 20 RPS sustained; return 429 with JSON-RPC error.
- [ ] Move secrets from .env to cloud secret manager when hosted.
- [ ] Basic pen-test scope and pre-GA checklist.

---

## G) Observability

**Goal:** Actionable metrics, dashboards, and alerts.

**Implementation tasks:**
- [ ] server/metrics/prom.ts — expose /metrics for Prometheus.
- [ ] infra/grafana/dashboards/mcp.json — dashboards for p50/p95 latency, throughput, errors, quota blocks.
- [ ] Alerts for error-rate, latency, and 5xx spikes.

---

## H) Packaging & GTM

**Goal:** Clear pricing and enterprise story to convert users.

**Implementation tasks:**
- [ ] Read pricing from config/pricing_calculator.json for site and API docs.
- [ ] Enterprise one-pager: SSO/SAML, RBAC, HA/SLA, VPC/on-prem, audit, backups.
- [ ] Demo repo/example: Kalliste + Qdrant + MindsDB agent flow and a single make target (make mem) to run.
- [ ] Reference docs: monetization_roadmap.md, icp_playbook.md, pilot_offer.md, outreach_templates.md.
- [ ] Reference docs: target_list.md, roi_calculator.md, pilot_landing.md.

---

## I) Release Management

**Goal:** Reproducible deployments and CI validation.

**Implementation tasks:**
- [ ] Freeze compose snapshot: infra/compose/.compose.lock.yml.
- [ ] CI pipeline:
  - Validate: docker compose -f .compose.lock.yml config
  - Bring up: docker compose -f .compose.lock.yml up -d --remove-orphans
  - Smoke-test: POST to :53130/memorymcp/mcp with Accept: application/json, text/event-stream and MCP-Protocol-Version: 2025-11-25

---

## J) Licensing & Hosting Model (quick note)

- If you want an open repo but protection for the hosted cloud, consider BSL 1.1 (source-available; restricts production managed hosting; converts to Apache-2.0 at Change Date).  
- If you require OSI-approved open-source with network copyleft, consider AGPLv3 and sell commercial exceptions for hosted deployments.

---

## Canonical HTTP-only MCP Shape (for tests & clients)

Example POST (replace host/port as needed):

    curl -fsS http://127.0.0.1:53130/memorymcp/mcp \
      -H 'accept: application/json, text/event-stream' \
      -H 'content-type: application/json' \
      -H 'MCP-Protocol-Version: 2025-11-25' \
      -d '{"jsonrpc":"2.0","id":"tools-1","method":"tools/list","params":{}}'

---

## Notes & references (for maintainers / agents)

- MCP HTTP/Streamable HTTP requires POSTing JSON-RPC to the single MCP endpoint and the client must include Accept: application/json, text/event-stream. The server may reply with application/json or initiate an SSE stream. See the MCP transports docs for details. (Reference: Model Context Protocol transports docs.)
- Implement metrics, logging, and billing carefully; meter calls and storage from day one to avoid retrofitting.
