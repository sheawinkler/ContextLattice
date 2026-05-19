#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

contextlattice_env
project="${CONTEXTLATTICE_PROJECT:-contextlattice}"
topic="${CONTEXTLATTICE_COMPACTION_TOPIC_PATH:-runbooks/context-compaction-handoff}"
query="${CONTEXTLATTICE_COMPACTION_QUERY:-context compaction handoff mission objective goal blockers next actions}"
timeout="${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-30}"
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

out="$(curl_json POST "${base}/memory/search" "$payload" "$timeout")"
python3 - "$out" <<'PY'
import json, sys
p = json.loads(sys.argv[1])
results = p.get("results") or []
print(json.dumps({
  "ok": bool(results) and not bool(p.get("degraded")),
  "hook": "post_compaction_read",
  "result_count": len(results),
  "first_file": results[0].get("file") if results and isinstance(results[0], dict) else None,
  "degraded": bool(p.get("degraded")),
}, separators=(",",":")))
PY
