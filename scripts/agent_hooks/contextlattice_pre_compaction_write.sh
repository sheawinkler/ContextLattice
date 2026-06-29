#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"
TOOL_ROOT="$(contextlattice_root)"

emit_codex_precompact_output() {
  local status="${1:-0}"
  python3 -c '
import json
import os
import sys

try:
    status = int(sys.argv[1])
except Exception:
    status = 1
raw = sys.stdin.read()
payload = {}
parse_error = None
if raw.strip():
    try:
        parsed = json.loads(raw)
        if isinstance(parsed, dict):
            payload = parsed
        else:
            parse_error = "non_object_output"
    except Exception as exc:
        parse_error = str(exc)

ok = status == 0 and parse_error is None and bool(payload.get("ok", True))
out = {
    "continue": True,
    "suppressOutput": ok,
}
if not ok:
    detail = parse_error or payload.get("error") or payload.get("reason") or f"exit_status_{status}"
    detail = " ".join(str(detail).split())[:240]
    out["systemMessage"] = f"ContextLattice PreCompact checkpoint did not complete cleanly: {detail}"
repo_root = sys.argv[2] if len(sys.argv) > 2 else ""
if repo_root:
    sys.path.insert(0, os.path.join(repo_root, "scripts"))
try:
    from agent_contracts import enforce_contract_limits
    out = enforce_contract_limits("codex_compact_hook_stdout.v1", out)
except Exception:
    pass
try:
    max_bytes = int(os.getenv("CONTEXTLATTICE_PRECOMPACT_OUTPUT_MAX_BYTES", "4096") or "4096")
except Exception:
    max_bytes = 4096
max_bytes = max(256, min(max_bytes, 65536))
encoded = json.dumps(out, separators=(",", ":"))
if len(encoded.encode("utf-8")) > max_bytes:
    out = {
        "continue": True,
        "suppressOutput": False,
        "systemMessage": "ContextLattice PreCompact checkpoint output exceeded local hook budget and was truncated.",
    }
    encoded = json.dumps(out, separators=(",", ":"))
print(encoded)
' "$status" "$TOOL_ROOT" || printf '%s\n' '{"continue":true,"suppressOutput":false,"systemMessage":"ContextLattice PreCompact checkpoint output emitter failed."}'
}

summary="${1:-${CONTEXTLATTICE_COMPACTION_SUMMARY:-}}"
summary="${summary:-continue objective-aligned execution after context compaction}"
project="${CONTEXTLATTICE_PROJECT:-contextlattice}"
topic="${CONTEXTLATTICE_COMPACTION_TOPIC_PATH:-runbooks/context-compaction-handoff}"
mode="${CONTEXTLATTICE_COMPACTION_RETRIEVAL_MODE:-balanced}"
query="${CONTEXTLATTICE_COMPACTION_QUERY:-context compaction handoff mission objective goal blockers next actions}"
export CONTEXTLATTICE_CLIENT_TIMEOUT_SECS="${CONTEXTLATTICE_CLIENT_TIMEOUT_SECS:-${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-200}}"

if [[ -z "${CONTEXTLATTICE_SESSION_ID:-}" && "${CONTEXTLATTICE_AUTO_SESSION_DISABLED:-0}" != "1" ]]; then
  set +e
  session_out="$(python3 "${TOOL_ROOT}/scripts/agent/contextlattice-session" ensure \
    "$summary" \
    --project "$project" \
    --agent "${CONTEXTLATTICE_AGENT:-compact-hook}" \
    --agent-id "${CONTEXTLATTICE_AGENT_ID:-codex_gpt5}" \
    --tag compaction \
    --tag auto-session \
    --metadata-json '{"hook":"pre_compaction"}' 2>/dev/null)"
  session_status=$?
  set -e
  if [[ "$session_status" == "0" && -n "$session_out" ]]; then
    session_id="$(python3 - "$session_out" <<'PY'
import json, sys
try:
    payload = json.loads(sys.argv[1])
except Exception:
    payload = {}
print(str(payload.get("session_id") or ""))
PY
)"
    if [[ -n "$session_id" ]]; then
      export CONTEXTLATTICE_SESSION_ID="$session_id"
    fi
  fi
fi

if [[ "${CONTEXTLATTICE_PRECOMPACT_SCHEMA_SMOKE:-}" == "1" ]]; then
  printf '%s' '{"ok":true}' | emit_codex_precompact_output 0
  exit 0
fi

set +e
handoff_payload="$(python3 "${TOOL_ROOT}/scripts/agent/compaction-handoff-payload" --fallback-summary "${summary}" --summary)"
payload_status=$?
set -e
if [[ "$payload_status" -ne 0 ]]; then
  printf '{"ok":false,"reason":"handoff_payload_failed"}' | emit_codex_precompact_output "$payload_status"
  exit 0
fi

set +e
orchestration_out="$(python3 "${TOOL_ROOT}/scripts/agent_orchestration.py" \
  compaction-handoff \
  "$project" \
  "$handoff_payload" \
  "$topic" \
  "$mode" \
  "$query")"
orchestration_status=$?
set -e
printf '%s' "$orchestration_out" | emit_codex_precompact_output "$orchestration_status"
