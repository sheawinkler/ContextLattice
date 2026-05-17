#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
source "${SCRIPT_DIR}/common.sh"

usage() {
  cat <<'USAGE'
Usage: agent_start.sh [--agent <id>] [--project <name>] [--topic-path <path>] [--soft] [--compact]

Runs low-cost deterministic startup hooks for agents:
  1. resource pressure sampler
  2. git lane guard
  3. OrbStack/host-forward guard
  4. ContextLattice policy pack retrieval

This does not commit, launch, or mutate project data.
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
results+=("$(run_hook resource_pressure "${SCRIPT_DIR}/resource_pressure_guard.sh" "${soft_arg[@]}")")
results+=("$(run_hook git_lane "${SCRIPT_DIR}/git_lane_guard.sh")")
results+=("$(run_hook orbstack_forward "${SCRIPT_DIR}/orbstack_forward_guard.sh")")
results+=("$(run_hook policy_pack "${SCRIPT_DIR}/agent_policy_pack.sh" --agent "$AGENT" --project "$PROJECT" --topic-path "$TOPIC")")

python3 - "$COMPACT" "$SOFT" "${results[@]}" <<'PY'
import json, sys
compact = sys.argv[1] == '1'
soft = sys.argv[2] == '1'
items = [json.loads(x) for x in sys.argv[3:]]
strict_ok = all(item.get('ok') for item in items)
ok = strict_ok or soft
if compact:
    summary = {'ok': ok, 'soft': soft, 'strict_ok': strict_ok, 'hooks': [{'name': i['name'], 'ok': i['ok']} for i in items]}
    policy = next((i.get('payload') for i in items if i.get('name') == 'policy_pack'), None)
    if isinstance(policy, dict):
        summary['policy'] = {k: policy.get(k) for k in ('mission','objective','goal')}
        summary['retrieval'] = policy.get('retrieval')
    print(json.dumps(summary, separators=(',', ':')))
else:
    print(json.dumps({'ok': ok, 'soft': soft, 'strict_ok': strict_ok, 'hooks': items}, separators=(',', ':')))
raise SystemExit(0 if ok else 1)
PY
