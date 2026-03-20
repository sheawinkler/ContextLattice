#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ENV_FILE:-${ROOT_DIR}/.env}"

if [[ -f "${ENV_FILE}" ]]; then
  # shellcheck disable=SC1090
  source "${ENV_FILE}" >/dev/null 2>&1 || true
fi

ORCH_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-${MEMMCP_ORCHESTRATOR_URL:-http://127.0.0.1:8075}}"
DASHBOARD_URL="${DASHBOARD_URL:-http://127.0.0.1:3000}"
API_KEY="${CONTEXTLATTICE_ORCHESTRATOR_API_KEY:-${MEMMCP_ORCHESTRATOR_API_KEY:-}}"

open_url() {
  local url="$1"
  if command -v open >/dev/null 2>&1; then
    open "${url}" >/dev/null 2>&1 || true
    return 0
  fi
  if command -v xdg-open >/dev/null 2>&1; then
    xdg-open "${url}" >/dev/null 2>&1 || true
    return 0
  fi
  return 1
}

echo "== ContextLattice monitoring =="
echo "Orchestrator: ${ORCH_URL}"
echo "Dashboard:    ${DASHBOARD_URL}"
echo

if command -v curl >/dev/null 2>&1; then
  echo "-- /health"
  curl -fsS "${ORCH_URL%/}/health" || true
  echo
  echo
  echo "-- /status"
  if [[ -n "${API_KEY}" ]]; then
    curl -fsS -H "x-api-key: ${API_KEY}" "${ORCH_URL%/}/status" || true
  else
    echo "INFO: CONTEXTLATTICE_ORCHESTRATOR_API_KEY not set; skipping authenticated /status call."
  fi
  echo
  echo
fi

opened=0
if open_url "${DASHBOARD_URL}"; then
  opened=1
fi
if open_url "${ORCH_URL%/}/health"; then
  opened=1
fi

if [[ "${opened}" -eq 1 ]]; then
  echo "Opened monitoring URLs in your default browser."
else
  echo "Could not auto-open browser. Open these manually:"
  echo "  ${DASHBOARD_URL}"
  echo "  ${ORCH_URL%/}/health"
fi

