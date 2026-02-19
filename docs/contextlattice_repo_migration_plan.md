# ContextLattice Private Repo Migration Plan

This plan moves only production-relevant files into a new private repository named `ContextLattice`, while preserving full local-first functionality and keeping cutover risk low.

## Goals

- Keep only files needed to build, run, test, operate, and document the platform.
- Remove stale experiments, backups, ad-hoc artifacts, and machine-specific clutter.
- Preserve reproducible full and lite launches.
- Validate behavior with deterministic smoke and regression gates before final cutover.

## Non-Goals

- Rewriting architecture or changing runtime behavior during migration.
- Breaking file history for core runtime paths.
- Shipping new feature work in the same migration PR.

## Target Repository Layout

```text
ContextLattice/
  .github/
    workflows/
  config/
  configs/
  docker/
  docs/
    legal/
    public_overview/
    releases/
  infra/
    compose/
    caddy/
  memmcp-dashboard/
  scripts/
  services/
    orchestrator/
  src/
  mk/
  docker-compose.yml
  docker-compose.lite.yml
  docker-compose.release.lock.yml
  Makefile
  README.md
  SKILLS.md
  LICENSE
  CONTRIBUTING.md
  CODE_OF_CONDUCT.md
  SECURITY.md
  .env.example
  .gitignore
```

## Keep vs Drop Rules

### Keep

- Runtime compose files and Dockerfiles.
- Orchestrator service code and tests.
- Dashboard app and tests.
- Production scripts:
  - startup, health, retention, rehydrate, service update, backup/restore, cloud checks.
- All documentation needed for installation, operations, legal, and public overview sync.
- CI/CD definitions and security/policy files.

### Drop

- Backup folders and ad-hoc restore copies:
  - `docker_compose_backup/`
  - `development/docker-compose-bak/`
  - `development/makefile-bak/`
- Local runtime artifacts:
  - `logs/`, `tmp/`, `.pid` files, local venv dirs, cache dirs.
- Legacy or one-off personal tooling not required for core stack.
- Stale generated files not referenced by build or runtime.

## Migration Phases

### Phase 1: Inventory and Freeze

- Freeze feature work during migration window.
- Capture baseline:
  - `git rev-parse HEAD`
  - `gmake mem-up && gmake mem-ps`
  - health and telemetry snapshots.
- Export current file inventory:
  - `rg --files > reports/migration_file_inventory.txt`

### Phase 2: Curated Include List

- Build `scripts/repo_curate_manifest.txt` with explicit include globs.
- Add denylist for known clutter and machine-local artifacts.
- Validate manifest by dry-run copy to `tmp/contextlattice-staging/`.

### Phase 3: Staging Copy

- Create new private repo `ContextLattice`.
- Copy only curated files into staging via `rsync --files-from` manifest.
- Recreate symlink expectations (`infra/compose/.env` guidance in docs).

### Phase 4: Build and Runtime Gates

- In staging repo, run:
  - `gmake mem-up-core`
  - `gmake mem-up-lite`
  - `gmake mem-up` (full default)
- Validate:
  - `curl /health`
  - `curl /telemetry/fanout`
  - `curl /telemetry/retention`
- Validate dashboard boot and API connectivity.

### Phase 5: Test Gates

- Run orchestrator tests: `pytest services/orchestrator/tests -q`.
- Run dashboard tests: `cd memmcp-dashboard && npm test`.
- Run cloud option checks:
  - `gmake qdrant-cloud-check` (BYO creds)
- Run storage/retention smoke:
  - `scripts/fanout_outbox_gc.py --dry-run`
  - `scripts/retention_runner.sh --dry-run` (if supported)

### Phase 6: Docs and Ops Parity

- Confirm all runbooks reference valid paths.
- Confirm public overview sync script works from new root.
- Confirm legal/license docs are intact and linked from `README.md`.

### Phase 7: Cutover

- Protect `main` in new repo.
- Open PR: `migration/bootstrap-contextlattice`.
- Require checks:
  - compose launch smoke
  - orchestrator tests
  - dashboard tests
  - lints/static checks as configured
- Merge when all checks pass and local manual validation matches baseline.

### Phase 8: Post-Cutover Cleanup

- Archive old private repo branch as read-only backup.
- Publish migration notes and path changes for contributors.
- Tag first clean release in new repo (for example `v0.9.0-bootstrap`).

## Definition of Done

- New `ContextLattice` repo launches full and lite modes from clean clone.
- All required docs and scripts are present and path-correct.
- No local-only artifacts tracked.
- CI checks pass on main.
- Public overview sync still works.

## Recommended Follow-ups

1. Add a `scripts/migrate_to_contextlattice.sh` helper to automate staged copy and validation.
2. Add a CI workflow that runs the exact launch/test matrix from Phase 4 and 5 on every PR.
3. Add a `docs/repository_structure.md` reference so contributors keep the tree clean over time.
