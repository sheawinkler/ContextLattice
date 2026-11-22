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
  echo '{"continue":true,"suppressOutput":false,"systemMessage":"ContextLattice SessionStart hook pack not found; run contextlattice_adopt install --pretty."}'
  exit 0
fi

export CONTEXTLATTICE_ORCHESTRATOR_URL="${CONTEXTLATTICE_ORCHESTRATOR_URL:-http://127.0.0.1:8075}"
export MEMMCP_ORCHESTRATOR_URL="${MEMMCP_ORCHESTRATOR_URL:-$CONTEXTLATTICE_ORCHESTRATOR_URL}"
export CONTEXTLATTICE_AGENT_ID="${CONTEXTLATTICE_AGENT_ID:-codex_gpt5}"
export MEMMCP_AGENT_ID="${MEMMCP_AGENT_ID:-$CONTEXTLATTICE_AGENT_ID}"
export CONTEXTLATTICE_DOCKER_RUNTIME="${CONTEXTLATTICE_DOCKER_RUNTIME:-orbstack}"

output="$(mktemp "${TMPDIR:-/tmp}/contextlattice-start.XXXXXX")" || { printf '%s\n' '{"continue":true,"systemMessage":"ContextLattice startup check unavailable."}'; exit 0; }
trap 'rm -f -- "$output"' EXIT
hook_status=0
CONTEXTLATTICE_CODEX_HOOK_EVENT=SessionStart "$HOOK_SCRIPT" \
  --agent "$CONTEXTLATTICE_AGENT_ID" \
  --project "${CONTEXTLATTICE_HOOK_PROJECT:-contextlattice}" \
  --topic-path "${CONTEXTLATTICE_HOOK_TOPIC_PATH:-runbooks/codex-integration}" \
  --soft \
  --compact >"$output" || hook_status=$?

python3 - "$output" "$hook_status" <<'PY'
import json
import sys

out = {"continue": True, "suppressOutput": True}
try:
    with open(sys.argv[1], "rb") as stream:
        body = json.loads(stream.read(12_001))
    if not isinstance(body, dict) or body.get("ok") is not True or sys.argv[2] != "0":
        raise ValueError("unavailable")
    retrieval = body.get("retrieval") or {}
    hits = retrieval.get("top_results") or []
    rows = [{k: str(row[k])[:160] for k in ("file", "topic_path", "source") if k in row}
            for row in hits[:4] if isinstance(row, dict)]
    out["hookSpecificOutput"] = {"hookEventName": "SessionStart", "additionalContext": json.dumps(
        {"hook": "contextlattice_agent_start", "ok": True, "top_results": rows}, separators=(",", ":"))}
except (AttributeError, OSError, TypeError, ValueError, RecursionError):
    out["systemMessage"] = "ContextLattice startup check unavailable; the session can continue."
print(json.dumps(out, separators=(",", ":")))
PY
