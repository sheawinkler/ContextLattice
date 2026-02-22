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

## F) Distribution execution tracker
- [x] Create channel-by-channel launch tracker: `docs/publish_execution_tracker.md`
- [x] Generate channel copybook for synchronized posting: `docs/launch_channel_copybook.md`
- [x] Add modular launch generator service: `launch_service/README.md`
- [x] Generate docs from modular config: `python3 launch_service/generate_launch_docs.py --config launch_service/config/contextlattice.launch.json`
- [x] Add submission requirements matrix + automated preflight: `docs/submission_requirements.md`, `gmake submission-preflight`
- [ ] Run D-2 launch dry run against `docs/publish_execution_tracker.md` run-of-show.

## G) 24-hour launch sprint (Feb 20-21, 2026)
- [x] Compressed all channel publish times into 24 hours in `launch_service/config/contextlattice.launch.json`.
- [x] Marked all channel entries as `Scheduled` in generated tracker docs.
- [ ] Domain routing cutover: `contextlattice.io` canonical; 301 redirect `privatememorycorp.ai`, `privatememorycorp.com`, `privatememorycorp.io`, and `contextlattice.pro` to canonical.
- [ ] Run `python3 scripts/submission_preflight.py --online` before sending submissions.
- [ ] Execute pre-launch submissions on Feb 20, 2026: MCP Registry, Glama, PulseMCP, MCP.so, FutureTools, Futurepedia, Toolify.
- [ ] Execute launch-day run-of-show on Feb 21, 2026: release, Product Hunt, Show HN, X, LinkedIn, Reddit, Hugging Face, Dev.to.
- [ ] Post-launch checkpoint on Feb 21, 2026 by 15:00 MST: update tracker statuses + KPI snapshot.
