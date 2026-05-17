#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: agent_policy_pack.sh [--agent <id>] [--project <name>] [--topic-path <path>] [--query <text>] [--mode fast|balanced|deep]

Builds a compact policy/context package for agents: endpoint, identity,
mission/objective/goal, search guidance, and first retrieval evidence.
USAGE
}

AGENT="${CONTEXTLATTICE_AGENT_ID:-codex_gpt5}"
PROJECT="${CONTEXTLATTICE_PROJECT:-contextlattice}"
TOPIC="runbooks/codex-integration"
QUERY="codex preflight connectivity retrieval agent policy context package current objective guidance"
MODE="balanced"
TIMEOUT="${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-30}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --agent) AGENT="$2"; shift 2 ;;
    --project|-p) PROJECT="$2"; shift 2 ;;
    --topic-path|-t) TOPIC="$2"; shift 2 ;;
    --query|-q) QUERY="$2"; shift 2 ;;
    --mode|-m) MODE="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
contextlattice_env
export CONTEXTLATTICE_AGENT_ID="$AGENT" MEMMCP_AGENT_ID="$AGENT"
BASE="${CONTEXTLATTICE_ORCHESTRATOR_URL%/}"
payload="$(python3 - "$PROJECT" "$TOPIC" "$QUERY" "$AGENT" "$MODE" <<'PY'
import json, sys
project, topic, query, agent, mode = sys.argv[1:]
print(json.dumps({
  "project": project,
  "projectName": project,
  "topic_path": topic,
  "topicPath": topic,
  "query": query,
  "agent_id": agent,
  "include_grounding": True,
  "include_retrieval_debug": True,
  "retrieval_mode": mode,
  "retrieval_intent": "decision",
  "limit": 8,
}))
PY
)"
search_out="$(curl_json POST "${BASE}/memory/search" "$payload" "$TIMEOUT" || true)"
python3 - "$AGENT" "$PROJECT" "$TOPIC" "$MODE" "$BASE" "$search_out" <<'PY'
import json, sys
agent, project, topic, mode, base, raw = sys.argv[1:]
try:
    search = json.loads(raw) if raw else {}
except Exception as exc:
    search = {'degraded': True, 'error': str(exc)}
results = search.get('results') if isinstance(search, dict) else []
if not isinstance(results, list):
    results = []
pack = {
    'ok': not bool(search.get('degraded')),
    'agent_id': agent,
    'project': project,
    'topic_path': topic,
    'retrieval_mode': mode,
    'orchestrator_url': base,
    'mission': 'Compound knowledge across projects into better agent outcomes with less repeated inference.',
    'objective': 'Improve longitudinal recall, retrieval quality, and orchestration decisions over time.',
    'goal': 'Maximize useful context per token while preserving correctness, provenance, and latency discipline.',
    'usage': {
        'start': 'contextlattice_agent_start',
        'search': 'contextlattice_search -p <project> -t <topic_path> -m balanced "<query>"',
        'write': 'contextlattice_write -p <project> -t <topic_path> -f notes/<agent>/<topic>.md --stdin',
        'checkpoint': 'contextlattice_checkpoint --project <project> --topic-path <topic_path> --file notes/<agent>/checkpoint.md --stdin',
    },
    'retrieval': {
        'degraded': bool(search.get('degraded')),
        'warnings': search.get('warnings') or [],
        'result_count': len(results),
        'top_results': [
            {k: item.get(k) for k in ('file','topic_path','source','score','summary') if isinstance(item, dict) and k in item}
            for item in results[:5]
            if isinstance(item, dict)
        ],
    },
}
print(json.dumps(pack, separators=(',', ':')))
PY
