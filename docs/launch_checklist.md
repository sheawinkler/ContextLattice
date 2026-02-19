# Launch checklist

This checklist tracks the concrete steps needed to launch ContextLattice across
local free, hosted cloud, and on‑prem tiers.

## A) Free local Starter rollout (default)
- [x] Confirm `.env.example` defaults stay permissive (`REQUIRE_ACTIVE_SUBSCRIPTION=false`, no API key).
- [x] Validate `scripts/first_run.sh` flow on a clean machine.
- [x] Ensure `docs/lite_profile.md` and README steps remain current.
- [x] Verify terminal dashboard (`scripts/terminal_dashboard.py`) output.

## B) Hosted cloud (paid)
- [x] Provision Postgres, Redis, object storage, Qdrant, MindsDB.
- [x] Configure secrets (billing provider keys, OAuth/SSO, orchestrator key).
- [x] Deploy orchestrator + memorymcp + mcp-hub + qdrant + mindsdb + dashboard.
- [x] Enable TLS and publish `/status`.
- [x] Run DB migrations + seed plans.
- [x] Enable billing webhooks + reconciliation jobs.
- [x] Turn on enforcement (`REQUIRE_ACTIVE_SUBSCRIPTION=true`, `MEMMCP_ORCHESTRATOR_API_KEY`).
- [x] Run `scripts/security_preflight.sh` with production env values.
- [x] Activate monitoring + alert thresholds.
- [x] Onboard pilot users and validate baseline usage savings.

## C) On‑prem lite (small teams)
- [x] Package `docker-compose.lite.yml` + `configs/mcp-hub.lite.json`.
- [x] Publish lite setup docs and offline runbook.
- [x] Decide license key or offline verification (if needed).

## D) On‑prem full (enterprise)
- [x] Package full compose bundle + env templates.
- [x] Decide SSO/OIDC requirements + SCIM expectations.
- [x] Provide upgrade + backup runbooks (see `docs/onprem_full_runbook.md`).
- [x] Confirm retention/export policies for regulated clients.

## E) GTM + sales motion
- [x] Finalize ICP messaging and CTA copy.
- [x] Align pricing page + public landing page.
- [x] Build pilot onboarding + ROI case study flow.
- [x] Prepare standard contract + license language.
