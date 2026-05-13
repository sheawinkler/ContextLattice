#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

python3 scripts/storage_ledger_capture.py \
  --orchestrator-url "${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}" \
  --keep-days "${ORCH_STORAGE_LEDGER_KEEP_DAYS:-180}" \
  --max-bytes "${ORCH_STORAGE_LEDGER_MAX_BYTES:-134217728}" \
  --tracked-top-limit "${ORCH_STORAGE_LEDGER_TRACKED_TOP_LIMIT:-24}" \
  --timeout-secs "${ORCH_STORAGE_LEDGER_TIMEOUT_SECS:-20}"
