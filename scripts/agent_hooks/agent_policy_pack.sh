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
DEFAULT_TIMEOUT_SECS=200
REQUESTED_TIMEOUT="${CONTEXTLATTICE_HOOK_TIMEOUT_SECS:-200}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --agent) AGENT="$2"; shift 2 ;;
    --project|-p) PROJECT="$2"; shift 2 ;;
    --topic-path|-t) TOPIC="$2"; shift 2 ;;
    --query|-q) QUERY="$2"; shift 2 ;;
    --mode|-m) MODE="$2"; shift 2 ;;
    --timeout) REQUESTED_TIMEOUT="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
contextlattice_env
export CONTEXTLATTICE_AGENT_ID="$AGENT" MEMMCP_AGENT_ID="$AGENT"
BASE="${CONTEXTLATTICE_ORCHESTRATOR_URL%/}"
if TIMEOUT="$(python3 - "$REQUESTED_TIMEOUT" <<'PY'
import math
import sys

try:
    requested = float(sys.argv[1])
except (TypeError, ValueError):
    raise SystemExit(1)
if not math.isfinite(requested) or requested <= 0:
    raise SystemExit(1)
print(requested)
PY
)"; then
  TIMEOUT_VALID=true
else
  TIMEOUT="$DEFAULT_TIMEOUT_SECS"
  TIMEOUT_VALID=false
fi
EFFECTIVE_DEADLINE="$(python3 - "$TIMEOUT" <<'PY'
from datetime import datetime, timedelta, timezone
import sys

seconds = float(sys.argv[1])
try:
    deadline = datetime.now(timezone.utc) + timedelta(seconds=seconds)
except OverflowError:
    print('unrepresentable')
else:
    print(deadline.isoformat().replace("+00:00", "Z"))
PY
)"
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
set +e
search_out="$(curl_json POST "${BASE}/memory/search" "$payload" "$TIMEOUT")"
curl_status=$?
set -e
search_transport_ok=true
if [[ "$curl_status" -ne 0 ]]; then
  search_transport_ok=false
fi
python3 - "$AGENT" "$PROJECT" "$TOPIC" "$MODE" "$BASE" "$search_transport_ok" "$curl_status" "$REQUESTED_TIMEOUT" "$TIMEOUT" "$EFFECTIVE_DEADLINE" "$TIMEOUT_VALID" "$DEFAULT_TIMEOUT_SECS" 3<<<"$search_out" <<'PY'
import json, math, os, sys
agent, project, topic, mode, base, transport_ok_raw, curl_status_raw, requested_timeout_raw, effective_timeout_raw, effective_deadline, timeout_valid_raw, default_timeout_raw = sys.argv[1:]
raw = os.fdopen(3).read()
transport_ok = transport_ok_raw == 'true'
try:
    curl_status = int(curl_status_raw)
except ValueError:
    curl_status = 1
try:
    requested_timeout = float(requested_timeout_raw)
    if not math.isfinite(requested_timeout):
        requested_timeout = None
except ValueError:
    requested_timeout = None
try:
    effective_timeout = float(effective_timeout_raw)
except ValueError:
    effective_timeout = 200.0
try:
    default_timeout = float(default_timeout_raw)
except ValueError:
    default_timeout = 200.0
timeout_valid = timeout_valid_raw == 'true'

def watchdog_payload(status, error_type=''):
    if not timeout_valid:
        repair_hint = 'Set CONTEXTLATTICE_HOOK_TIMEOUT_SECS or --timeout to a finite positive number; the 200-second repository default was used.'
    elif status != 'completed':
        repair_hint = 'Retry contextlattice_agent_start after the local gateway is healthy; do not replay the POST blindly.'
    else:
        repair_hint = ''
    return {
        'schema_id': 'contextlattice_agent_policy_watchdog.v1',
        'status': status,
        'bounded': True,
        'one_shot': True,
        'deadline_enforced': True,
        'requested_timeout_secs': requested_timeout,
        'effective_timeout_secs': effective_timeout,
        'effective_deadline': effective_deadline,
        'default_timeout_secs': default_timeout,
        'timeout_validation': 'valid' if timeout_valid else 'invalid_fell_back_to_default',
        'curl_exit_code': curl_status,
        'error_type': error_type or None,
        'repair_hint': repair_hint,
    }

def degraded_search(message, error_type='transport_failure'):
    return {
        'ok': False,
        'degraded': True,
        'result_state': 'degraded',
        'partial': False,
        'error': {
            'type': error_type,
            'message': message,
            'curl_exit_code': curl_status,
        },
        'warnings': [f'{message}; continue in degraded-memory mode.'],
    }

def invalid_response_shape(search):
    typed_fields = (
        ('ok', bool),
        ('degraded', bool),
        ('result_state', str),
        ('partial', bool),
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
    if curl_status in (28, 124):
        search = degraded_search('ContextLattice retrieval request failed (watchdog timeout)', 'watchdog_timeout')
    else:
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
partial = search.get('partial') is True or result_state == 'partial'
results = search.get('results')
if not isinstance(results, list):
    if not degraded:
        search = degraded_search('ContextLattice retrieval returned an invalid response shape')
        degraded = True
        partial = False
    results = []
warnings = list(search.get('warnings') or [])
if degraded and not any('degraded-memory mode' in warning for warning in warnings):
    warnings.append('ContextLattice retrieval is degraded; continue in degraded-memory mode.')
pack = {
    'ok': not degraded,
    'status': 'degraded' if degraded else ('partial' if partial else 'ready'),
    'result_state': 'degraded' if degraded else ('partial' if partial else (result_state or 'ready')),
    'partial': False if degraded else partial,
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
        'partial': False if degraded else partial,
        'warnings': warnings,
        'result_count': len(results),
        'top_results': [
            {k: item.get(k) for k in ('file','topic_path','source','score','summary') if isinstance(item, dict) and k in item}
            for item in results[:5]
            if isinstance(item, dict)
        ],
    },
    'watchdog': watchdog_payload('failed' if not transport_ok else 'completed', 'watchdog_timeout' if curl_status in (28, 124) else ''),
}
if isinstance(search.get('error'), dict):
    pack['error'] = search['error']
print(json.dumps(pack, separators=(',', ':')))
PY
