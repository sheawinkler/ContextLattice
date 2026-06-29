#!/usr/bin/env bash
# Shared helpers for ContextLattice agent hooks.

set -euo pipefail

hook_name() {
  basename "$0" .sh
}

hook_source_root() {
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd
}

is_contextlattice_root() {
  local root="$1"
  [[ -n "$root" ]] || return 1
  [[ -f "${root}/config/agents/agent_profiles.json" && -d "${root}/scripts/agent_hooks" ]]
}

contextlattice_root() {
  local source_root
  source_root="$(hook_source_root)"
  if [[ -n "${CONTEXTLATTICE_REPO_ROOT:-}" ]] && is_contextlattice_root "$CONTEXTLATTICE_REPO_ROOT"; then
    cd "$CONTEXTLATTICE_REPO_ROOT" && pwd
    return 0
  fi
  if is_contextlattice_root "$source_root"; then
    printf '%s\n' "$source_root"
    return 0
  fi
  if git rev-parse --show-toplevel >/dev/null 2>&1; then
    local git_root
    git_root="$(git rev-parse --show-toplevel)"
    if is_contextlattice_root "$git_root"; then
      printf '%s\n' "$git_root"
      return 0
    fi
  fi
  printf '%s\n' "$source_root"
}

repo_root() {
  if git rev-parse --show-toplevel >/dev/null 2>&1; then
    git rev-parse --show-toplevel
  else
    contextlattice_root
  fi
}

log() {
  printf '[%s] %s\n' "$(hook_name)" "$*" >&2
}

fail() {
  printf '[%s] FAIL: %s\n' "$(hook_name)" "$*" >&2
  exit 1
}

json_string() {
  python3 - "$1" <<'PY'
import json, sys
print(json.dumps(sys.argv[1]))
PY
}

emit_json_kv() {
  python3 - "$@" <<'PY'
import json, sys
pairs = sys.argv[1:]
out = {}
for pair in pairs:
    if '=' not in pair:
        continue
    key, value = pair.split('=', 1)
    if value in ('true', 'false'):
        out[key] = value == 'true'
    else:
        try:
            out[key] = int(value)
        except ValueError:
            out[key] = value
print(json.dumps(out, separators=(',', ':')))
PY
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

contextlattice_env() {
  export CONTEXTLATTICE_ORCHESTRATOR_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
  export MEMMCP_ORCHESTRATOR_URL="${MEMMCP_ORCHESTRATOR_URL:-$CONTEXTLATTICE_ORCHESTRATOR_URL}"
  export CONTEXTLATTICE_AGENT_ID="${CONTEXTLATTICE_AGENT_ID:-codex_gpt5}"
  export MEMMCP_AGENT_ID="${MEMMCP_AGENT_ID:-$CONTEXTLATTICE_AGENT_ID}"
  if [[ -z "${CONTEXTLATTICE_ORCHESTRATOR_API_KEY:-}" ]]; then
    local repo_env key_value
    repo_env="$(contextlattice_root)/.env"
    if [[ -f "$repo_env" ]]; then
      key_value="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1); exit}' "$repo_env")"
      if [[ -n "$key_value" ]]; then
        export CONTEXTLATTICE_ORCHESTRATOR_API_KEY="$key_value"
      fi
    fi
  fi
}

curl_json() {
  local method="$1"
  local url="$2"
  local data="${3:-}"
  local timeout="${4:-20}"
  local auth_args=()
  if [[ -n "${CONTEXTLATTICE_ORCHESTRATOR_API_KEY:-}" ]]; then
    auth_args=(-H "x-api-key: ${CONTEXTLATTICE_ORCHESTRATOR_API_KEY}")
  fi
  if [[ -n "$data" ]]; then
    curl -fsS -m "$timeout" -X "$method" "$url" -H 'Content-Type: application/json' ${auth_args[@]+"${auth_args[@]}"} --data-binary "$data"
  else
    curl -fsS -m "$timeout" -X "$method" "$url" ${auth_args[@]+"${auth_args[@]}"}
  fi
}
