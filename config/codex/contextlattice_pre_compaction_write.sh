#!/usr/bin/env bash
set -euo pipefail

for env_file in "$HOME/.contextlattice/agent_hooks.env" "$HOME/.codex/contextlattice_hooks.env"; do
  if [[ -f "$env_file" ]]; then
    # shellcheck source=/dev/null
    source "$env_file"
  fi
done

HOOK_SCRIPT=""
REPO_ROOT="${CONTEXTLATTICE_REPO_ROOT:-}"
if [[ -x "$HOME/.contextlattice/scripts/agent_hooks/contextlattice_pre_compaction_write.sh" ]]; then
  HOOK_SCRIPT="$HOME/.contextlattice/scripts/agent_hooks/contextlattice_pre_compaction_write.sh"
elif [[ -n "$REPO_ROOT" && -x "$REPO_ROOT/scripts/agent_hooks/contextlattice_pre_compaction_write.sh" ]]; then
  HOOK_SCRIPT="$REPO_ROOT/scripts/agent_hooks/contextlattice_pre_compaction_write.sh"
else
  for candidate in "$HOME/Documents/Projects/ContextLattice" "$HOME/ContextLattice"; do
    if [[ -x "$candidate/scripts/agent_hooks/contextlattice_pre_compaction_write.sh" ]]; then
      HOOK_SCRIPT="$candidate/scripts/agent_hooks/contextlattice_pre_compaction_write.sh"
      break
    fi
  done
fi

if [[ -z "$HOOK_SCRIPT" ]]; then
  echo '{"continue":true,"suppressOutput":false,"systemMessage":"ContextLattice PreCompact hook pack not found; run contextlattice_adopt install --pretty from a current ContextLattice checkout"}'
  exit 0
fi

export CONTEXTLATTICE_ORCHESTRATOR_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
export MEMMCP_ORCHESTRATOR_URL="${MEMMCP_ORCHESTRATOR_URL:-$CONTEXTLATTICE_ORCHESTRATOR_URL}"
export CONTEXTLATTICE_PROJECT="${CONTEXTLATTICE_HOOK_PROJECT:-${CONTEXTLATTICE_PROJECT:-contextlattice}}"
export CONTEXTLATTICE_DOCKER_RUNTIME="${CONTEXTLATTICE_DOCKER_RUNTIME:-orbstack}"

exec "$HOOK_SCRIPT" "$@"
