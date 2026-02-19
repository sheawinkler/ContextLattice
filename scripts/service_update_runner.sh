#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

LOCK_DIR="${SERVICE_UPDATE_LOCK_DIR:-tmp/service-update.lockdir}"
mkdir -p "$(dirname "$LOCK_DIR")"

if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  echo "[service-update-runner] lock active; another run is already in progress"
  exit 0
fi
cleanup() {
  rmdir "$LOCK_DIR" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Keep defaults conservative for unattended runs.
export APPLY_UPDATES="${APPLY_UPDATES:-1}"
export ALLOW_MAJOR_UPDATES="${ALLOW_MAJOR_UPDATES:-0}"
export REDEPLOY_AFTER_UPDATE="${REDEPLOY_AFTER_UPDATE:-1}"
export REDEPLOY_SCOPE="${REDEPLOY_SCOPE:-changed}"
export RUN_UNIT_TESTS="${RUN_UNIT_TESTS:-1}"
export RUN_SMOKE_TESTS="${RUN_SMOKE_TESTS:-1}"

if [[ -f .env ]]; then
  set -a
  # shellcheck source=/dev/null
  source .env
  set +a
fi

echo "[service-update-runner] start $(date -u +%Y-%m-%dT%H:%M:%SZ)"
bash scripts/update_services_pipeline.sh
echo "[service-update-runner] done $(date -u +%Y-%m-%dT%H:%M:%SZ)"
