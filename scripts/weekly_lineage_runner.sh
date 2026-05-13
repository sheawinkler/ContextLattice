#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

if [[ -f "$ROOT_DIR/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$ROOT_DIR/.env"
  set +a
fi

python3 scripts/weekly_lineage_rollup.py \
  --orchestrator-url "${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}" \
  --memory-root "${GO_MEMORY_STORE_ROOT:-${MEMORY_BANK_ROOT:-/tmp/contextlattice-memory-bank}}" \
  --out-root "${CONTEXTLATTICE_LINEAGE_ROOT:-./.data/cold/lineage}" \
  --min-count "${CONTEXTLATTICE_LINEAGE_MIN_COUNT:-1}" \
  --top-topic-limit "${CONTEXTLATTICE_LINEAGE_TOP_TOPIC_LIMIT:-60}" \
  --synergy-min-projects "${CONTEXTLATTICE_LINEAGE_SYNERGY_MIN_PROJECTS:-2}" \
  --keep-weeks "${CONTEXTLATTICE_LINEAGE_KEEP_WEEKS:-104}" \
  --emit-synergy
