#!/usr/bin/env bash
set -euo pipefail

TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
NATIVE_TOOL="${TOOL_HOME}/bin/contextlattice-agent-tools"
if [[ ! -x "$NATIVE_TOOL" ]]; then
  printf '%s\n' 'Missing native ContextLattice tools. Run scripts/install_global_agent_tools.sh first.' >&2
  exit 1
fi

# Old shared hook files can still supply transport defaults. Source them only
# in the subshell and read back only these fields. In particular, neither
# file is allowed to supply or replace the caller's actor, model, or session.
checkpoint_transport_defaults() {
  local env_file
  for env_file in "${TOOL_HOME}/agent_hooks.env" "${HOME:-}/.codex/contextlattice_hooks.env"; do
    if [[ -f "$env_file" ]]; then
      # shellcheck source=/dev/null
      source "$env_file" >/dev/null
    fi
  done
  printf '%s\0' "${CONTEXTLATTICE_REPO_ROOT:-}" \
    "${CONTEXTLATTICE_ORCHESTRATOR_URL:-${MEMMCP_ORCHESTRATOR_URL:-}}" \
    "${CONTEXTLATTICE_ORCHESTRATOR_API_KEY:-}" "${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-}"
}

{
  IFS= read -r -d '' hook_repo
  IFS= read -r -d '' hook_url
  IFS= read -r -d '' hook_key
  IFS= read -r -d '' hook_timeout
} < <(checkpoint_transport_defaults)

CONTEXTLATTICE_REPO_ROOT="${CONTEXTLATTICE_REPO_ROOT:-$hook_repo}" \
CONTEXTLATTICE_ORCHESTRATOR_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-${MEMMCP_ORCHESTRATOR_URL:-$hook_url}}" \
CONTEXTLATTICE_ORCHESTRATOR_API_KEY="${CONTEXTLATTICE_ORCHESTRATOR_API_KEY-$hook_key}" \
CONTEXTLATTICE_HOOK_TIMEOUT_SECS="${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-$hook_timeout}" \
  exec "$NATIVE_TOOL" contextlattice_checkpoint "$@"
