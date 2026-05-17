#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: contextlattice_checkpoint.sh --project <name> --topic-path <path> --file <logical.md> [--content <text>|--content-file <path>|--stdin] [--query <text>]

Writes a checkpoint to /memory/write and verifies it with /memory/search.
USAGE
}

PROJECT="${CONTEXTLATTICE_PROJECT:-contextlattice}"
TOPIC=""
FILE=""
CONTENT=""
CONTENT_FILE=""
READ_STDIN=0
QUERY=""
TIMEOUT="${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-30}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --project|-p) PROJECT="$2"; shift 2 ;;
    --topic-path|-t) TOPIC="$2"; shift 2 ;;
    --file|-f) FILE="$2"; shift 2 ;;
    --content|-c) CONTENT="$2"; shift 2 ;;
    --content-file) CONTENT_FILE="$2"; shift 2 ;;
    --stdin) READ_STDIN=1; shift ;;
    --query|-q) QUERY="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
[[ -n "$PROJECT" ]] || fail "--project is required"
[[ -n "$TOPIC" ]] || fail "--topic-path is required"
[[ -n "$FILE" ]] || fail "--file is required"
if [[ -n "$CONTENT_FILE" ]]; then
  CONTENT="$(cat "$CONTENT_FILE")"
elif [[ "$READ_STDIN" == "1" ]]; then
  CONTENT="$(cat)"
fi
[[ -n "$CONTENT" ]] || fail "content is required"
[[ -n "$QUERY" ]] || QUERY="$CONTENT"

contextlattice_env
BASE="${CONTEXTLATTICE_ORCHESTRATOR_URL%/}"
write_payload="$(python3 - "$PROJECT" "$FILE" "$TOPIC" "$CONTENT" <<'PY'
import json, sys
project, file_name, topic, content = sys.argv[1:]
print(json.dumps({"projectName": project, "fileName": file_name, "topicPath": topic, "content": content, "agent_id": "codex_gpt5"}))
PY
)"
write_out="$(curl_json POST "${BASE}/memory/write" "$write_payload" "$TIMEOUT")"
search_payload="$(python3 - "$PROJECT" "$TOPIC" "$QUERY" <<'PY'
import json, sys
project, topic, query = sys.argv[1:]
print(json.dumps({
  "project": project,
  "projectName": project,
  "topic_path": topic,
  "topicPath": topic,
  "query": query[:500],
  "include_grounding": True,
  "include_retrieval_debug": True,
  "retrieval_mode": "balanced",
  "limit": 5,
}))
PY
)"
search_out="$(curl_json POST "${BASE}/memory/search" "$search_payload" "$TIMEOUT")"
python3 - "$write_out" "$search_out" <<'PY'
import json, sys
write = json.loads(sys.argv[1])
search = json.loads(sys.argv[2])
results = search.get('results') or []
ok = bool(write.get('ok', True)) and bool(results) and not bool(search.get('degraded'))
print(json.dumps({
    'ok': ok,
    'write': {'ok': write.get('ok', True), 'source': write.get('source'), 'content_ref': write.get('content_ref')},
    'readback': {'degraded': bool(search.get('degraded')), 'count': len(results), 'first_file': (results[0].get('file') if results and isinstance(results[0], dict) else None)},
}, separators=(',', ':')))
raise SystemExit(0 if ok else 1)
PY
