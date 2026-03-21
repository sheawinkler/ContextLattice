#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-$HOME/ContextLattice}"
BASE_URL="${BASE_URL:-http://127.0.0.1:8075}"
API_KEY="${API_KEY:-}"

usage() {
  cat <<USAGE
Usage: ContextLattice-Monitor.sh [--install-dir PATH] [--base-url URL] [--api-key KEY]
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --install-dir)
      INSTALL_DIR="$2"
      shift 2
      ;;
    --base-url)
      BASE_URL="$2"
      shift 2
      ;;
    --api-key)
      API_KEY="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown argument: $1" >&2
      usage
      exit 1
      ;;
  esac
done

if [[ -z "${API_KEY}" && -f "${INSTALL_DIR}/.env" ]]; then
  API_KEY="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' "${INSTALL_DIR}/.env" | tail -n 1)"
fi
if [[ -z "${API_KEY}" && -f "${INSTALL_DIR}/.env" ]]; then
  API_KEY="$(awk -F= '/^MEMMCP_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' "${INSTALL_DIR}/.env" | tail -n 1)"
fi

echo "== ContextLattice Monitor =="
echo

echo "/health"
curl -fsS "${BASE_URL%/}/health" | jq .

if [[ -n "${API_KEY}" ]]; then
  echo
  echo "/status"
  curl -fsS -H "x-api-key: ${API_KEY}" "${BASE_URL%/}/status" | jq .

  echo
  echo "/telemetry/fanout"
  curl -fsS -H "x-api-key: ${API_KEY}" "${BASE_URL%/}/telemetry/fanout" | jq .
else
  echo
  echo "WARN: API key not found; skipping authenticated checks."
fi

if command -v xdg-open >/dev/null 2>&1; then
  xdg-open "http://127.0.0.1:3000" >/dev/null 2>&1 || true
fi
