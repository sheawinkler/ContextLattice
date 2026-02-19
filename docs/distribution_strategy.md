# Distribution strategy

We will keep a private master repo as the source of truth, then publish
customer-facing repos derived from it.

## Proposed tiers

1. **Public overview repo** (already): README + marketing only.
2. **On‑prem lite repo**: `docker-compose.lite.yml` + minimal docs.
3. **On‑prem full repo**: full compose + observability + MindsDB/Langfuse.
4. **Hosted cloud repo** (private): infra IaC + deployment manifests.

## How syncing should work

We recommend keeping everything in the private master repo and exporting
specific subsets to downstream repos:

- **Option A (simple):** `rsync` export + `git commit` in each downstream repo.
- **Option B (scalable):** `git subtree` or `git filter-repo` for each tier.

## Suggested structure

- `docker-compose.lite.yml` + `configs/mcp-hub.lite.json` → lite repo
- `docker-compose.yml` + `configs/` + `docs/` → full on‑prem repo
- `infra/` + `deploy/` + `helm/` (future) → hosted cloud repo

We can automate exports once you decide the target repo names and paths.
