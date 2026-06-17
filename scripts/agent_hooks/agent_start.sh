#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: agent_start.sh [--agent <id>] [--project <name>] [--topic-path <path>] [--soft] [--compact]

Runs low-cost deterministic startup hooks for agents:
  0. agent runtime session ensure/recovery
  1. Codex session-store doctor
  2. resource pressure sampler
  3. git lane guard
  4. OrbStack/host-forward guard
  5. native endpoint smoke
  6. recall monitor seed
  7. ContextLattice policy pack retrieval

This does not commit or launch. It may record a bounded agent runtime session.
USAGE
}

AGENT="${CONTEXTLATTICE_AGENT_ID:-codex_gpt5}"
PROJECT="${CONTEXTLATTICE_PROJECT:-contextlattice}"
TOPIC="runbooks/codex-integration"
SOFT=0
COMPACT=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --agent) AGENT="$2"; shift 2 ;;
    --project|-p) PROJECT="$2"; shift 2 ;;
    --topic-path|-t) TOPIC="$2"; shift 2 ;;
    --soft) SOFT=1; shift ;;
    --compact) COMPACT=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done

contextlattice_env
REPO_ROOT="$(repo_root)"
if [[ -z "${CONTEXTLATTICE_SESSION_ID:-}" && "${CONTEXTLATTICE_AUTO_SESSION_DISABLED:-0}" != "1" ]]; then
  set +e
  session_out="$(python3 "${REPO_ROOT}/scripts/agent/contextlattice-session" ensure \
    "Agent startup for ${PROJECT} at ${TOPIC}" \
    --project "$PROJECT" \
    --agent "$AGENT" \
    --agent-id "$AGENT" \
    --tag agent-start \
    --tag auto-session \
    --metadata-json '{"hook":"agent_start"}' 2>/dev/null)"
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

run_hook() {
  local name="$1"
  shift
  local out code
  set +e
  out="$("$@" 2>&1)"
  code=$?
  set -e
  if [[ "$code" != "0" && "$SOFT" != "1" ]]; then
    printf '%s\n' "$out" >&2
    fail "${name} failed"
  fi
  python3 - "$name" "$code" "$out" <<'PY'
import json, sys
name, code, out = sys.argv[1], int(sys.argv[2]), sys.argv[3]
parsed = None
try:
    parsed = json.loads(out.splitlines()[-1]) if out.strip() else None
except Exception:
    parsed = None
print(json.dumps({'name': name, 'ok': code == 0, 'exit_code': code, 'payload': parsed, 'raw_tail': out[-1200:] if parsed is None else None}, separators=(',', ':')))
PY
}

results=()
soft_arg=()
[[ "$SOFT" == "1" ]] && soft_arg=(--soft)
results+=("$(run_hook codex_session_store "${REPO_ROOT}/scripts/agent/audit-codex-session-store")")
results+=("$(run_hook resource_pressure "${SCRIPT_DIR}/resource_pressure_guard.sh" ${soft_arg[@]+"${soft_arg[@]}"})")
results+=("$(run_hook git_lane "${SCRIPT_DIR}/git_lane_guard.sh")")
results+=("$(run_hook orbstack_forward "${SCRIPT_DIR}/orbstack_forward_guard.sh")")
results+=("$(run_hook native_smoke "${SCRIPT_DIR}/native_endpoint_smoke.sh" --project "$PROJECT" ${soft_arg[@]+"${soft_arg[@]}"})")
results+=("$(run_hook recall_seed "${SCRIPT_DIR}/recall_monitor_seed.sh" ${soft_arg[@]+"${soft_arg[@]}"})")
results+=("$(run_hook policy_pack "${SCRIPT_DIR}/agent_policy_pack.sh" --agent "$AGENT" --project "$PROJECT" --topic-path "$TOPIC")")

python3 - "$COMPACT" "$SOFT" "${results[@]}" <<'PY'
import json, os, sys
compact = sys.argv[1] == '1'
soft = sys.argv[2] == '1'
items = [json.loads(x) for x in sys.argv[3:]]
strict_ok = all(item.get('ok') for item in items)
ok = strict_ok or soft
if compact:
    summary = {'ok': ok, 'soft': soft, 'strict_ok': strict_ok, 'session_id': os.getenv('CONTEXTLATTICE_SESSION_ID', ''), 'hooks': [{'name': i['name'], 'ok': i['ok']} for i in items]}
    policy = next((i.get('payload') for i in items if i.get('name') == 'policy_pack'), None)
    if isinstance(policy, dict):
        summary['policy'] = {k: policy.get(k) for k in ('mission','objective','goal')}
        summary['retrieval'] = policy.get('retrieval')
    session_store = next((i.get('payload') for i in items if i.get('name') == 'codex_session_store'), None)
    if isinstance(session_store, dict):
        findings = session_store.get('findings') or []
        summary['codex_session_store'] = {
            'ok': session_store.get('ok'),
            'advanced_mode': session_store.get('advanced_mode'),
            'external_volume': session_store.get('external_volume'),
            'tcc_managed': session_store.get('tcc_managed'),
            'sessions_realpath': session_store.get('sessions_realpath'),
            'warning_count': session_store.get('warning_count', 0),
            'error_count': session_store.get('error_count', 0),
            'findings': [
                {
                    'severity': f.get('severity'),
                    'reason': f.get('reason'),
                    'path': f.get('path'),
                }
                for f in findings[:4]
                if isinstance(f, dict)
            ],
        }
    print(json.dumps(summary, separators=(',', ':')))
else:
    print(json.dumps({'ok': ok, 'soft': soft, 'strict_ok': strict_ok, 'session_id': os.getenv('CONTEXTLATTICE_SESSION_ID', ''), 'hooks': items}, separators=(',', ':')))
raise SystemExit(0 if ok else 1)
PY
