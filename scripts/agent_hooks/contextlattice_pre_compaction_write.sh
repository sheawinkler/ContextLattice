#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

stdin_payload=""
if [[ ! -t 0 ]]; then
  stdin_payload="$(cat || true)"
fi

summary="${1:-${CONTEXTLATTICE_COMPACTION_SUMMARY:-}}"
if [[ -z "${summary}" && -n "${stdin_payload}" ]]; then
  summary="$(python3 - "${stdin_payload}" <<'PY'
import json, sys
raw = sys.argv[1]
try:
    payload = json.loads(raw)
except Exception:
    print(raw[:1200])
    raise SystemExit(0)

def strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for key in ("summary", "task_summary", "objective", "goal", "prompt", "message", "reason"):
            if isinstance(value.get(key), str):
                yield value[key]
        for item in value.values():
            yield from strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from strings(item)

for text in strings(payload):
    text = " ".join(text.split())
    if text:
        print(text[:1200])
        break
else:
    print(raw[:1200])
PY
)"
fi
summary="${summary:-continue objective-aligned execution after context compaction}"
project="${CONTEXTLATTICE_PROJECT:-contextlattice}"
topic="${CONTEXTLATTICE_COMPACTION_TOPIC_PATH:-runbooks/context-compaction-handoff}"
mode="${CONTEXTLATTICE_COMPACTION_RETRIEVAL_MODE:-balanced}"
query="${CONTEXTLATTICE_COMPACTION_QUERY:-context compaction handoff mission objective goal blockers next actions}"

python3 "${REPO_ROOT}/scripts/agent_orchestration.py" \
  compaction-handoff \
  "$project" \
  "$summary" \
  "$topic" \
  "$mode" \
  "$query" \
  | python3 -c 'import json,sys; p=json.load(sys.stdin); print(json.dumps({"ok": bool(p.get("ok", True)), "hook": "pre_compaction_write", "file": p.get("file"), "topic_path": p.get("topic_path")}, separators=(",",":")))'
