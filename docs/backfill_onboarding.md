# Backfill And Onboarding Sync

Use this when a user already has data in memory-bank and/or imported Qdrant, and you want all sinks (Mongo, Qdrant, MindsDB, Letta) to converge.

## Scripts

- `scripts/qdrant_migrate_from_mongo.sh`
  - Creates/cuts over a new Qdrant collection (new dimension) and backfills from Mongo raw in a controlled way.
- `scripts/mindsdb_rotate_rehydrate.sh`
  - Rotates MindsDB autosync DB/table and rehydrates from Mongo raw and/or memory-bank.
- `scripts/rehydrate_fanout.sh`
  - Rehydrates fanout from memory-bank files.
- `scripts/rehydrate_when_quiet.sh`
  - Waits for fanout load to drop, then triggers memory-bank rehydrate.
- `scripts/backfill_existing_data.sh`
  - Three-phase backfill:
    - Mongo raw source backfill (controlled queue pressure).
    - Memory-bank source rehydrate.
    - Qdrant-source backfill via orchestrator endpoint.

## Recommended full backfill

```bash
WAIT_QUIET=true \
FORCE_REQUEUE=false \
RUN_MONGO_RAW=true \
MONGO_RAW_LIMIT=120000 \
MEMORY_BANK_LIMIT=5000 \
QDRANT_LIMIT=50000 \
scripts/backfill_existing_data.sh
```

## Qdrant dimension migration + backfill from Mongo raw

```bash
scripts/qdrant_migrate_from_mongo.sh \
  --target memmcp_notes_v2_768 \
  --dim 768 \
  --recreate \
  --limit 120000 \
  --max-pending 6000
```

## MindsDB rotate + full rehydrate

```bash
scripts/mindsdb_rotate_rehydrate.sh \
  --db files_repair_$(date -u +%Y%m%d%H%M%S) \
  --table memory_events \
  --source both \
  --mongo-limit 150000 \
  --memory-limit 20000
```

## Targeted project backfill (quiet window)

```bash
PROJECT=algotraderv2_rust \
WAIT_QUIET=true \
RUN_MONGO_RAW=true \
MEMORY_BANK_LIMIT=2000 \
QDRANT_LIMIT=10000 \
scripts/backfill_existing_data.sh
```

## Notes

- Qdrant-source backfill uses summary-level records when original full content is unavailable.
- Mongo-raw backfill uses original `content_raw` and is preferred when available.
- Some MindsDB rows may fail permanently due to malformed legacy payloads; those are marked `failed` instead of looping retries.
- If qdrant backlog is heavy, set `MINDSDB_FANOUT_WORKERS=1` (or higher) so MindsDB rehydrate is not starved.
- Track progress from `GET /telemetry/fanout`.
