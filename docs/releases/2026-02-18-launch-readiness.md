# Release Notes - 2026-02-18 Launch Readiness

## Summary

This release finalizes public-beta launch hardening and packaging:

- Qdrant cloud/local smoke coverage validated.
- Launch readiness gate automated (load, drain, backup/restore, security preflight).
- Reproducible deployment lockfile added for pinned image digests.
- Legal + licensing package finalized for hosted/commercial onboarding.
- Public messaging package prepared.

## Runtime and reliability

- Added API-key support to `scripts/load_test_memory_write.py`.
- Added `scripts/launch_backup_restore_drill.sh` for Mongo backup/restore + Qdrant snapshot validation.
- Added `scripts/launch_readiness_gate.sh` for an integrated launch gate run.
- Added Make targets:
  - `launch-readiness-gate`
  - `backup-restore-drill`
  - `mem-up-release`
  - `mem-up-lite-release`
  - `release-lock-verify`

## Deployment reproducibility

- Added `docker-compose.release.lock.yml` pinning external service images to known digest values.

## MindsDB fanout resilience

- Added `MINDSDB_FAIL_OPEN_ON_PERMANENT_ERROR` (default `true`).
- Permanent MindsDB sink errors now fail-open for fanout durability:
  - write path remains durable via memory-bank + Mongo raw + Qdrant,
  - no new failed deadletters for deterministic MindsDB corruption signatures.

## Documentation

- Legal package:
  - `docs/legal/README.md`
  - `docs/legal/COMMERCIAL_LICENSE.md`
  - `docs/legal/TERMS_OF_SERVICE.md`
  - `docs/legal/PRIVACY_POLICY.md`
  - `docs/legal/DPA.md`
  - `docs/legal/ACCEPTABLE_USE_POLICY.md`
  - `docs/legal/SUBPROCESSORS.md`
- Public messaging:
  - `docs/public_messaging_package.md`
- Surface expansion roadmap:
  - `docs/messaging_surface_expansion.md`

## Known risk

- Existing historical MindsDB deadletters may remain from earlier runs.
- Recommended cleanup:
  1. Rehydrate only if required.
  2. Run outbox GC for aged failed rows.
  3. Keep fail-open enabled for deterministic permanent sink errors.
