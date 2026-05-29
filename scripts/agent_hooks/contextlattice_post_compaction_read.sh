#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"
REPO_ROOT="$(repo_root)"

emit_codex_postcompact_output() {
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
    out["systemMessage"] = f"ContextLattice PostCompact checkpoint read did not complete cleanly: {detail}"
repo_root = sys.argv[2] if len(sys.argv) > 2 else ""
if repo_root:
    sys.path.insert(0, os.path.join(repo_root, "scripts"))
try:
    from agent_contracts import enforce_contract_limits
    out = enforce_contract_limits("codex_compact_hook_stdout.v1", out)
except Exception:
    pass
try:
    max_bytes = int(os.getenv("CONTEXTLATTICE_POSTCOMPACT_OUTPUT_MAX_BYTES", "4096") or "4096")
except Exception:
    max_bytes = 4096
max_bytes = max(256, min(max_bytes, 65536))
encoded = json.dumps(out, separators=(",", ":"))
if len(encoded.encode("utf-8")) > max_bytes:
    out = {
        "continue": True,
        "suppressOutput": False,
        "systemMessage": "ContextLattice PostCompact checkpoint output exceeded local hook budget and was truncated.",
    }
    encoded = json.dumps(out, separators=(",", ":"))
print(encoded)
' "$status" "$REPO_ROOT" || printf '%s\n' '{"continue":true,"suppressOutput":false,"systemMessage":"ContextLattice PostCompact checkpoint output emitter failed."}'
}

if [[ "${CONTEXTLATTICE_POSTCOMPACT_SCHEMA_SMOKE:-}" == "1" ]]; then
  printf '%s' '{"ok":true}' | emit_codex_postcompact_output 0
  exit 0
fi

contextlattice_env
project="${CONTEXTLATTICE_PROJECT:-contextlattice}"
topic="${CONTEXTLATTICE_COMPACTION_TOPIC_PATH:-runbooks/context-compaction-handoff}"
query="${CONTEXTLATTICE_COMPACTION_QUERY:-}"
if [[ -z "${query}" && ! -t 0 ]]; then
  set +e
  query="$(python3 "${SCRIPT_DIR}/../agent/compaction-handoff-payload" --query)"
  query_status=$?
  set -e
  if [[ "$query_status" -ne 0 ]]; then
    query=""
  fi
fi
query="${query:-context compaction handoff mission objective goal blockers next actions}"
timeout="${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-200}"
base="${CONTEXTLATTICE_ORCHESTRATOR_URL%/}"

payload="$(python3 - "$project" "$topic" "$query" <<'PY'
import json, sys
project, topic, query = sys.argv[1:]
print(json.dumps({
  "project": project,
  "projectName": project,
  "topic_path": topic,
  "topicPath": topic,
  "query": query,
  "retrieval_mode": "balanced",
  "include_grounding": True,
  "include_retrieval_debug": True,
  "limit": 5,
}))
PY
)"

set +e
out="$(curl_json POST "${base}/memory/search" "$payload" "$timeout")"
curl_status=$?
set -e
if [[ "$curl_status" -ne 0 ]]; then
  printf '{"ok":false,"reason":"memory_search_failed"}' | emit_codex_postcompact_output "$curl_status"
  exit 0
fi

set +e
post_out="$(python3 - "$out" <<'PY'
import json, sys
p = json.loads(sys.argv[1])
results = p.get("results") or []
out = {
  "ok": bool(results) and not bool(p.get("degraded")),
  "hook": "post_compaction_read",
  "result_count": len(results),
  "first_file": results[0].get("file") if results and isinstance(results[0], dict) else None,
  "degraded": bool(p.get("degraded")),
}
print(json.dumps(out, separators=(",",":")))
PY
)"
post_status=$?
set -e
printf '%s' "$post_out" | emit_codex_postcompact_output "$post_status"
