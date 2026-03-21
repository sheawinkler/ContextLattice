#!/usr/bin/env bash
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/sheawinkler/ContextLattice.git}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/ContextLattice}"
FULL_MODE="${FULL_MODE:-0}"

usage() {
  cat <<USAGE
Usage: ContextLattice-Install.sh [--full] [--repo-url URL] [--install-dir PATH]

Options:
  --full                 Start full compose stack (default is lite)
  --repo-url URL         Override git repository URL
  --install-dir PATH     Override install directory (default: ~/ContextLattice)
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --full)
      FULL_MODE="1"
      shift
      ;;
    --repo-url)
      REPO_URL="$2"
      shift 2
      ;;
    --install-dir)
      INSTALL_DIR="$2"
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

require_cmd() {
  local name="$1"
  local hint="$2"
  if ! command -v "$name" >/dev/null 2>&1; then
    echo "ERROR: ${name} is required. ${hint}" >&2
    exit 1
  fi
}

gen_key() {
  if command -v openssl >/dev/null 2>&1; then
    printf 'cl_%s' "$(openssl rand -hex 24)"
  else
    printf 'cl_%s' "$(date +%s%N)"
  fi
}

set_env_value() {
  local file="$1"
  local key="$2"
  local value="$3"
  if [[ ! -f "$file" ]]; then
    printf '%s=%s\n' "$key" "$value" > "$file"
    return
  fi
  if rg -q "^${key}=" "$file"; then
    perl -0777 -i -pe "s#^${key}=.*#${key}=${value}#m" "$file"
  else
    printf '%s=%s\n' "$key" "$value" >> "$file"
  fi
}

echo "== ContextLattice Linux Installer =="
echo "Repo: ${REPO_URL}"
echo "Install dir: ${INSTALL_DIR}"

require_cmd git "Install git and rerun."
require_cmd docker "Install Docker Engine/Desktop (with Compose v2) and rerun."

if [[ ! -d "${INSTALL_DIR}/.git" ]]; then
  echo "Cloning repository..."
  git clone "${REPO_URL}" "${INSTALL_DIR}"
else
  echo "Updating existing repository..."
  git -C "${INSTALL_DIR}" pull --ff-only || echo "WARN: git pull failed; continuing with local checkout."
fi

cd "${INSTALL_DIR}"

if [[ ! -f .env ]]; then
  cp .env.example .env
fi

key="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env | tail -n 1)"
if [[ -z "${key}" ]]; then
  key="$(gen_key)"
fi

set_env_value .env CONTEXTLATTICE_ORCHESTRATOR_API_KEY "${key}"
set_env_value .env MEMMCP_ORCHESTRATOR_API_KEY "${key}"
set_env_value .env CONTEXTLATTICE_ORCHESTRATOR_URL "http://127.0.0.1:8075"
set_env_value .env MEMMCP_ORCHESTRATOR_URL "http://127.0.0.1:8075"
set_env_value .env HOST_BIND_ADDRESS "127.0.0.1"
set_env_value .env CONTEXTLATTICE_ENV "production"
set_env_value .env ORCH_SECURITY_STRICT "true"

compose_file="docker-compose.lite.yml"
if [[ "${FULL_MODE}" == "1" ]]; then
  compose_file="docker-compose.yml"
fi

echo "Launching stack with ${compose_file} ..."
docker compose -f "${compose_file}" up -d --build

echo "Waiting for API health..."
for i in {1..30}; do
  if curl -fsS "http://127.0.0.1:8075/health" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

echo
curl -fsS "http://127.0.0.1:8075/health" || true
echo

if curl -fsS -H "x-api-key: ${key}" "http://127.0.0.1:8075/status" >/dev/null 2>&1; then
  echo "Status check: ok"
else
  echo "WARN: status endpoint not ready yet (retry with Monitor script)."
fi

echo
echo "Install complete."
echo "Run ContextLattice-Monitor.sh for health + telemetry checks."

echo "API URL: http://127.0.0.1:8075"
echo "Dashboard: http://127.0.0.1:3000"

if command -v xdg-open >/dev/null 2>&1; then
  xdg-open "http://127.0.0.1:3000" >/dev/null 2>&1 || true
fi
