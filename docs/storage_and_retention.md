# Storage & retention

This repo is designed to run locally via Docker Desktop. If you hit Qdrant errors like:
- `no space left on device` (often in WAL)

…you typically need to either (A) expand Docker Desktop's disk image, or (B) move heavy data to host/external storage, or both.

## 1) Expand Docker Desktop disk image (fastest unblock)

Docker Desktop → Settings → Resources → Storage
- Increase **Disk image size** (example: 256–512 GB)
- Restart Docker Desktop

This immediately fixes disk-pressure when you are using **named volumes** inside Docker Desktop's disk image.

### Optional: move Docker Desktop VM disk to external SSD (macOS)

If local SSD pressure is still high, move Docker Desktop `DataFolder` to your external drive:

```bash
scripts/docker_desktop_move_datafolder.sh \
  --target /Volumes/ExternalSSD/docker-desktop-data \
  --apply
```

Dry-run first:

```bash
scripts/docker_desktop_move_datafolder.sh \
  --target /Volumes/ExternalSSD/docker-desktop-data
```

## 2) Put Qdrant + Mongo + memory bank on host / external SSD

`docker-compose.yml` supports overriding volume *sources* via env vars.

By default it uses named volumes (e.g. `qdrant_storage`).
If you set the env var to a **host path** (e.g. `/Volumes/ExternalSSD/...`) it becomes a bind mount.

### 2.1 Recommended directory layout

Example:
- `/Volumes/ExternalSSD/contextlattice/qdrant`
- `/Volumes/ExternalSSD/contextlattice/mongo`
- `/Volumes/ExternalSSD/contextlattice/memory-bank`
- `/Volumes/ExternalSSD/contextlattice/orchestrator`

Create them:

```bash
mkdir -p /Volumes/ExternalSSD/contextlattice/{qdrant,mongo,memory-bank,orchestrator}
```

## Memory bank retention + export

- Export memory files: `scripts/memory_bank_export.sh`
- Purge old memory files: `scripts/memory_bank_purge.sh` (defaults to 90-day retention)
- Automated runs: `scripts/retention_runner.sh` (see `docs/retention_ops.md`)

### 2.2 Configure via `.env`

Copy the example env:

```bash
cp .env.example .env
```

Then set these (paths are just examples):

```ini
QDRANT_STORAGE=/Volumes/ExternalSSD/contextlattice/qdrant
MONGO_DATA=/Volumes/ExternalSSD/contextlattice/mongo
MEMORY_BANK_DATA=/Volumes/ExternalSSD/contextlattice/memory-bank
ORCHESTRATOR_DATA=/Volumes/ExternalSSD/contextlattice/orchestrator
```

### 2.3 Docker Desktop file sharing (macOS)

If you mount anything under `/Volumes/...`, ensure Docker Desktop is allowed to access it:
Docker Desktop → Settings → Resources → File Sharing

If the external drive is not present, Docker may fail to start containers that depend on those mounts.

## 3) Retention + cold storage

### 3.1 Orchestrator telemetry/history NDJSON

The orchestrator persists simple NDJSON histories (trading/strategies/overrides/signals) under the `ORCHESTRATOR_DATA` volume.

To keep a hot window (example: 48h) and archive the rest to gzipped cold storage:

```bash
python3 scripts/archive_ndjson_by_time.py \
  --data-dir /Volumes/ExternalSSD/contextlattice/orchestrator \
  --retention-hours 48 \
  --cold-dir /Volumes/ExternalSSD/contextlattice/cold/telemetry
```

### 3.2 Qdrant snapshots + pruning

New Qdrant points written by the orchestrator include a numeric `payload.ts` field (epoch seconds) to enable time-based retention.

You can snapshot a collection to cold storage and then prune points older than N days:

```bash
python3 scripts/qdrant_snapshot_prune.py \
  --qdrant-url http://localhost:6333 \
  --collection memmcp_notes \
  --retention-days 14 \
  --snapshot-dir /Volumes/ExternalSSD/contextlattice/cold/qdrant

# Optional hour-level pruning (overrides days)
python3 scripts/qdrant_snapshot_prune.py \
  --qdrant-url http://localhost:6333 \
  --collection memmcp_notes \
  --retention-hours 1 \
  --snapshot-dir /Volumes/ExternalSSD/contextlattice/cold/qdrant
```

For hourly retention runs where duplicate snapshot files are too expensive, set:

```ini
QDRANT_SKIP_SNAPSHOT=1
```

Note: pruning only removes points that have `payload.ts`. Older points without `ts` are left untouched.

### 3.3 Monitoring

Use the read-only audit script to spot runaway growth:

```bash
python3 scripts/storage_audit.py --qdrant-url http://localhost:6333 --mongo-url mongodb://localhost:27017 --mongo-db memorybank
```
