#!/usr/bin/env bash
set -euo pipefail

# Codex SessionStart hook: compact, non-blocking ContextLattice orientation.
# It intentionally runs soft because the app may be restarting while Codex opens.

for env_file in "$HOME/.contextlattice/agent_hooks.env" "$HOME/.codex/contextlattice_hooks.env"; do
  if [[ -f "$env_file" ]]; then
    # shellcheck source=/dev/null
    source "$env_file"
  fi
done

HOOK_SCRIPT=""
REPO_ROOT="${CONTEXTLATTICE_REPO_ROOT:-}"
if [[ -x "$HOME/.contextlattice/scripts/agent_hooks/agent_start.sh" ]]; then
  HOOK_SCRIPT="$HOME/.contextlattice/scripts/agent_hooks/agent_start.sh"
elif [[ -n "$REPO_ROOT" && -x "$REPO_ROOT/scripts/agent_hooks/agent_start.sh" ]]; then
  HOOK_SCRIPT="$REPO_ROOT/scripts/agent_hooks/agent_start.sh"
else
  for candidate in "$HOME/Documents/Projects/ContextLattice" "$HOME/ContextLattice"; do
    if [[ -x "$candidate/scripts/agent_hooks/agent_start.sh" ]]; then
      HOOK_SCRIPT="$candidate/scripts/agent_hooks/agent_start.sh"
      break
    fi
  done
fi

if [[ -z "$HOOK_SCRIPT" ]]; then
  echo '{"ok":false,"hook":"contextlattice_agent_start","reason":"ContextLattice hook pack not found; run contextlattice_adopt install --pretty from a current ContextLattice checkout"}'
  exit 0
fi

export CONTEXTLATTICE_ORCHESTRATOR_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
export MEMMCP_ORCHESTRATOR_URL="${MEMMCP_ORCHESTRATOR_URL:-$CONTEXTLATTICE_ORCHESTRATOR_URL}"
export CONTEXTLATTICE_AGENT_ID="${CONTEXTLATTICE_AGENT_ID:-codex_gpt5}"
export MEMMCP_AGENT_ID="${MEMMCP_AGENT_ID:-$CONTEXTLATTICE_AGENT_ID}"
export CONTEXTLATTICE_DOCKER_RUNTIME="${CONTEXTLATTICE_DOCKER_RUNTIME:-orbstack}"

"$HOOK_SCRIPT" \
  --agent "$CONTEXTLATTICE_AGENT_ID" \
  --project "${CONTEXTLATTICE_HOOK_PROJECT:-contextlattice}" \
  --topic-path "${CONTEXTLATTICE_HOOK_TOPIC_PATH:-runbooks/codex-integration}" \
  --soft \
  --compact
