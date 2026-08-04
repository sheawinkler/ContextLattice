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
import json, os, sys
project, topic, query, agent, mode = sys.argv[1:]
print(json.dumps({
  "project": project,
  "projectName": project,
  "topic_path": topic,
  "topicPath": topic,
  "query": query,
  "agent_id": agent,
  "session_id": os.getenv("CONTEXTLATTICE_SESSION_ID", ""),
  "include_grounding": True,
  "include_retrieval_debug": True,
  "retrieval_mode": mode,
  "retrieval_intent": "decision",
  "limit": 8,
}))
PY
)"
search_transport_ok=true
if ! search_out="$(curl_json POST "${BASE}/memory/search" "$payload" "$TIMEOUT")"; then
  search_transport_ok=false
fi
python3 - "$AGENT" "$PROJECT" "$TOPIC" "$MODE" "$BASE" "$search_transport_ok" 3<<<"$search_out" <<'PY'
import json, os, sys
agent, project, topic, mode, base, transport_ok_raw = sys.argv[1:]
raw = os.fdopen(3).read()
transport_ok = transport_ok_raw == 'true'

def degraded_search(message):
    return {
        'degraded': True,
        'warnings': [f'{message}; continue in degraded-memory mode.'],
    }

def invalid_response_shape(search):
    typed_fields = (
        ('ok', bool),
        ('degraded', bool),
        ('result_state', str),
    )
    for key, expected_type in typed_fields:
        if key in search and not isinstance(search[key], expected_type):
            return True
    if 'warnings' in search:
        warnings = search['warnings']
        if not isinstance(warnings, list) or any(not isinstance(item, str) for item in warnings):
            return True
    results = search.get('results')
    if isinstance(results, list) and any(not isinstance(item, dict) for item in results):
        return True
    return False

if not transport_ok:
    search = degraded_search('ContextLattice retrieval request failed')
elif not raw.strip():
    search = degraded_search('ContextLattice retrieval returned an empty response')
else:
    try:
        search = json.loads(raw)
    except Exception:
        search = degraded_search('ContextLattice retrieval returned invalid JSON')
if not isinstance(search, dict):
    search = degraded_search('ContextLattice retrieval returned an invalid response shape')
elif invalid_response_shape(search):
    search = degraded_search('ContextLattice retrieval returned an invalid response shape')
result_state = search.get('result_state', '').strip().lower()
degraded = search.get('degraded') is True or search.get('ok') is False or result_state == 'degraded'
results = search.get('results')
if not isinstance(results, list):
    if not degraded:
        search = degraded_search('ContextLattice retrieval returned an invalid response shape')
        degraded = True
    results = []
warnings = list(search.get('warnings') or [])
if degraded and not any('degraded-memory mode' in warning for warning in warnings):
    warnings.append('ContextLattice retrieval is degraded; continue in degraded-memory mode.')
pack = {
    'ok': not degraded,
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
        'degraded': degraded,
        'warnings': warnings,
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
