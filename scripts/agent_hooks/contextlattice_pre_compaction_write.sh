#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

summary="${1:-${CONTEXTLATTICE_COMPACTION_SUMMARY:-continue objective-aligned execution after context compaction}}"
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
