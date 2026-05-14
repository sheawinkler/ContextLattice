#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

scripts/context_storage_ops.sh weekly-lineage \
  --orchestrator-url "${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}" \
  --min-count "${CONTEXTLATTICE_LINEAGE_MIN_COUNT:-1}" \
  --top-topic-limit "${CONTEXTLATTICE_LINEAGE_TOP_TOPIC_LIMIT:-60}" \
  --synergy-min-projects "${CONTEXTLATTICE_LINEAGE_SYNERGY_MIN_PROJECTS:-2}" \
  --keep-weeks "${CONTEXTLATTICE_LINEAGE_KEEP_WEEKS:-104}" \
  --emit-synergy
