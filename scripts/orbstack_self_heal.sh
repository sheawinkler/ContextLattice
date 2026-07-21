#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

CMD="${1:-run-once}"
if [[ "$CMD" != "run-once" && "$CMD" != "start" && "$CMD" != "stop" && "$CMD" != "status" ]]; then
  echo "usage: scripts/orbstack_self_heal.sh [run-once|start|stop|status] [--event name]" >&2
  exit 2
fi
shift || true

EVENT="manual"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --event)
      EVENT="${2:-manual}"
      shift 2
      ;;
    *)
      echo "unknown arg: $1" >&2
      exit 2
      ;;
  esac
done

RUNTIME_DIR="${CONTEXTLATTICE_ORBSTACK_HEAL_RUNTIME_DIR:-$ROOT_DIR/.data/runtime}"
LOG_FILE="${CONTEXTLATTICE_ORBSTACK_HEAL_LOG_FILE:-$RUNTIME_DIR/orbstack-self-heal.log}"
STATE_FILE="${CONTEXTLATTICE_ORBSTACK_HEAL_STATE_FILE:-$RUNTIME_DIR/orbstack-self-heal.state}"
LOCK_DIR="${CONTEXTLATTICE_ORBSTACK_HEAL_LOCK_DIR:-$RUNTIME_DIR/orbstack-self-heal.lock}"
LAUNCHD_LABEL="${CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCHD_LABEL:-io.contextlattice.orbstack-self-heal}"
LAUNCHD_PLIST="${CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCHD_PLIST:-$HOME/Library/LaunchAgents/${LAUNCHD_LABEL}.plist}"
INTERVAL_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_INTERVAL_SECS:-60}"
DOCKER_TIMEOUT_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_DOCKER_TIMEOUT_SECS:-5}"
HEALTH_TIMEOUT_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_HEALTH_TIMEOUT_SECS:-5}"
HEALTH_URL="${CONTEXTLATTICE_HEAL_ORCH_URL:-http://127.0.0.1:8075/health}"
DOCKER_CONTEXT_NAME="${DOCKER_CONTEXT:-orbstack}"
ALLOW_FORWARD_REPAIR="${CONTEXTLATTICE_ORBSTACK_HEAL_FORWARD_REPAIR:-1}"
FORWARD_REPAIR_COOLDOWN_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_FORWARD_REPAIR_COOLDOWN_SECS:-300}"
HEALTH_FAILURES_BEFORE_REPAIR="${CONTEXTLATTICE_ORBSTACK_HEAL_HEALTH_FAILURES_BEFORE_REPAIR:-3}"
ORCH_SERVICE_LABEL="${CONTEXTLATTICE_HEAL_ORCH_SERVICE_LABEL:-gateway-go}"
SHED_CPU_THRESHOLD="${CONTEXTLATTICE_ORBSTACK_HEAL_CONTAINER_CPU_THRESHOLD:-800}"
SHED_SERVICES="${CONTEXTLATTICE_ORBSTACK_HEAL_SHED_SERVICES:-}"
SHED_ACTION="${CONTEXTLATTICE_ORBSTACK_HEAL_SHED_ACTION:-restart}"
ALLOW_VM_RESTART="${CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART:-0}"
FAILURES_BEFORE_RESTART="${CONTEXTLATTICE_ORBSTACK_HEAL_FAILURES_BEFORE_RESTART:-5}"
STARTUP_GRACE_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_STARTUP_GRACE_SECS:-300}"
POST_RESTART_GRACE_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_POST_RESTART_GRACE_SECS:-300}"
RESTART_COOLDOWN_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_COOLDOWN_SECS:-900}"
RESTART_WINDOW_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_RESTART_WINDOW_SECS:-3600}"
MAX_RESTARTS_PER_WINDOW="${CONTEXTLATTICE_ORBSTACK_HEAL_MAX_RESTARTS_PER_WINDOW:-1}"
RESTART_READY_ATTEMPTS="${CONTEXTLATTICE_ORBSTACK_HEAL_RESTART_READY_ATTEMPTS:-30}"
RESTART_READY_INTERVAL_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_RESTART_READY_INTERVAL_SECS:-2}"

normalize_uint() {
  local name="$1" default_value="$2" value
  value="${!name}"
  if [[ ! "$value" =~ ^[0-9]+$ ]]; then
    value="$default_value"
  fi
  while [[ "${#value}" -gt 1 && "$value" == 0* ]]; do
    value="${value#0}"
  done
  printf -v "$name" '%s' "$value"
}

normalize_positive_uint() {
  local name="$1" default_value="$2"
  normalize_uint "$name" "$default_value"
  if (( ${!name} < 1 )); then
    printf -v "$name" '%s' "$default_value"
  fi
}

normalize_number() {
  local name="$1" default_value="$2" value
  value="${!name}"
  if [[ ! "$value" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
    value="$default_value"
  fi
  printf -v "$name" '%s' "$value"
}

normalize_bool() {
  local name="$1" value
  value="${!name}"
  if [[ "$value" != "1" ]]; then
    value="0"
  fi
  printf -v "$name" '%s' "$value"
}

normalize_positive_uint INTERVAL_SECS 60
normalize_number DOCKER_TIMEOUT_SECS 5
normalize_number HEALTH_TIMEOUT_SECS 5
normalize_uint FORWARD_REPAIR_COOLDOWN_SECS 300
normalize_positive_uint HEALTH_FAILURES_BEFORE_REPAIR 3
normalize_number SHED_CPU_THRESHOLD 800
normalize_bool ALLOW_FORWARD_REPAIR
normalize_bool ALLOW_VM_RESTART
normalize_positive_uint FAILURES_BEFORE_RESTART 5
normalize_uint STARTUP_GRACE_SECS 300
normalize_uint POST_RESTART_GRACE_SECS 300
normalize_uint RESTART_COOLDOWN_SECS 900
normalize_positive_uint RESTART_WINDOW_SECS 3600
normalize_uint MAX_RESTARTS_PER_WINDOW 1
normalize_positive_uint RESTART_READY_ATTEMPTS 30
normalize_number RESTART_READY_INTERVAL_SECS 2

mkdir -p "$RUNTIME_DIR"

json_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  value="${value//$'\r'/\\r}"
  value="${value//$'\t'/\\t}"
  printf '%s' "$value"
}

json_emit() {
  local first=1 pair key value
  printf '{'
  for pair in "$@"; do
    [[ "$pair" == *=* ]] || continue
    key="${pair%%=*}"
    value="${pair#*=}"
    if [[ "$first" == "1" ]]; then
      first=0
    else
      printf ','
    fi
    printf '"%s":' "$(json_escape "$key")"
    if [[ "$value" == "true" || "$value" == "false" || "$value" == "null" ]]; then
      printf '%s' "$value"
    elif [[ "$value" =~ ^-?[0-9]+([.][0-9]+)?$ ]]; then
      printf '%s' "$value"
    else
      printf '"%s"' "$(json_escape "$value")"
    fi
  done
  printf '}\n'
}

log_line() {
  local event_value="$EVENT" message="$*"
  event_value="${event_value//$'\n'/ }"
  event_value="${event_value//$'\r'/ }"
  message="${message//$'\n'/ }"
  message="${message//$'\r'/ }"
  printf '%s event=%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$event_value" "$message" >> "$LOG_FILE"
}

read_state() {
  local key="$1"
  if [[ -f "$STATE_FILE" ]]; then
    awk -F= -v k="$key" '$1==k {print substr($0, index($0, "=")+1)}' "$STATE_FILE" | tail -1
  fi
}

state_uint() {
  local key="$1" default_value="${2:-0}" value
  value="$(read_state "$key")"
  if [[ ! "$value" =~ ^[0-9]+$ ]]; then
    value="$default_value"
  fi
  while [[ "${#value}" -gt 1 && "$value" == 0* ]]; do
    value="${value#0}"
  done
  printf '%s\n' "$value"
}

write_state() {
  local key="$1" value="$2" tmp
  tmp="$(mktemp "$STATE_FILE.tmp.XXXXXX")"
  if [[ -f "$STATE_FILE" ]]; then
    awk -F= -v k="$key" -v v="$value" '
      BEGIN { done=0 }
      $1==k { print k "=" v; done=1; next }
      { print }
      END { if (!done) print k "=" v }
    ' "$STATE_FILE" > "$tmp"
  else
    printf '%s=%s\n' "$key" "$value" > "$tmp"
  fi
  mv "$tmp" "$STATE_FILE"
}

release_lock() {
  rm -f "$LOCK_DIR/pid" >/dev/null 2>&1 || true
  rmdir "$LOCK_DIR" >/dev/null 2>&1 || true
}

acquire_lock() {
  local owner=""
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    printf '%s\n' "$$" > "$LOCK_DIR/pid"
    return 0
  fi
  if [[ -f "$LOCK_DIR/pid" ]]; then
    owner="$(cat "$LOCK_DIR/pid" 2>/dev/null || true)"
  fi
  if [[ "$owner" =~ ^[0-9]+$ ]] && kill -0 "$owner" 2>/dev/null; then
    return 1
  fi
  rm -f "$LOCK_DIR/pid" >/dev/null 2>&1 || true
  rmdir "$LOCK_DIR" >/dev/null 2>&1 || return 1
  mkdir "$LOCK_DIR" 2>/dev/null || return 1
  printf '%s\n' "$$" > "$LOCK_DIR/pid"
}

run_with_timeout() {
  local timeout="$1" output_file="$2" command_pid watchdog_pid status
  shift 2
  : > "$output_file"
  "$@" > "$output_file" 2>&1 &
  command_pid=$!
  (
    sleep "$timeout"
    if kill -0 "$command_pid" 2>/dev/null; then
      kill -TERM "$command_pid" 2>/dev/null || true
      sleep 1
      kill -KILL "$command_pid" 2>/dev/null || true
    fi
  ) </dev/null >/dev/null 2>&1 &
  watchdog_pid=$!
  set +e
  wait "$command_pid"
  status=$?
  set -e
  kill "$watchdog_pid" 2>/dev/null || true
  wait "$watchdog_pid" 2>/dev/null || true
  return "$status"
}

run_timeout() {
  local timeout="$1" tmp status=0
  shift
  tmp="$(mktemp "$RUNTIME_DIR/command.XXXXXX")"
  run_with_timeout "$timeout" "$tmp" "$@" || status=$?
  rm -f "$tmp"
  return "$status"
}

capture_timeout() {
  local timeout="$1" tmp status=0
  shift
  tmp="$(mktemp "$RUNTIME_DIR/command.XXXXXX")"
  run_with_timeout "$timeout" "$tmp" "$@" || status=$?
  if [[ "$status" == "0" ]]; then
    cat "$tmp"
  fi
  rm -f "$tmp"
  return "$status"
}

docker_server_ok() {
  command -v docker >/dev/null 2>&1 || return 1
  run_timeout "$DOCKER_TIMEOUT_SECS" docker --context "$DOCKER_CONTEXT_NAME" version --format '{{.Server.Version}}'
}

health_ok() {
  local body root_ok
  command -v curl >/dev/null 2>&1 || return 1
  body="$(curl -fsS --max-time "$HEALTH_TIMEOUT_SECS" "$HEALTH_URL" 2>/dev/null)" || return 1
  command -v plutil >/dev/null 2>&1 || return 1
  root_ok="$(printf '%s' "$body" | plutil -extract ok raw -o - - 2>/dev/null)" || return 1
  [[ "$root_ok" == "true" ]]
}

orbstack_cpu_pct() {
  ps -axo %cpu,command 2>/dev/null | awk '/OrbStack Helper.*vmgr/ {sum += $1} END {printf "%.1f\n", sum + 0}'
}

number_at_least() {
  awk -v value="$1" -v threshold="$2" 'BEGIN {exit !((value + 0) >= (threshold + 0))}'
}

container_exists() {
  local container="$1"
  run_timeout "$DOCKER_TIMEOUT_SECS" docker --context "$DOCKER_CONTEXT_NAME" inspect "$container"
}

container_cpu_pct() {
  local container="$1" output
  output="$(capture_timeout "$DOCKER_TIMEOUT_SECS" docker --context "$DOCKER_CONTEXT_NAME" stats --no-stream --format '{{.CPUPerc}}' "$container")" || {
    printf '0\n'
    return 0
  }
  printf '%s\n' "$output" | awk 'NF {gsub(/%/, "", $1); print $1; found=1; exit} END {if (!found) print "0"}'
}

container_control() {
  local container="$1"
  case "$SHED_ACTION" in
    stop)
      run_timeout "$DOCKER_TIMEOUT_SECS" docker --context "$DOCKER_CONTEXT_NAME" stop "$container"
      ;;
    restart)
      run_timeout "$DOCKER_TIMEOUT_SECS" docker --context "$DOCKER_CONTEXT_NAME" restart "$container"
      ;;
    *)
      log_line "action=shed_container skipped reason=invalid_action"
      return 1
      ;;
  esac
}

shed_high_cpu_containers() {
  local changed=0 raw part cpu
  [[ -n "$SHED_SERVICES" ]] || return 1
  raw="${SHED_SERVICES//,/ }"
  for part in $raw; do
    [[ -n "$part" ]] || continue
    container_exists "$part" || continue
    cpu="$(container_cpu_pct "$part")"
    if number_at_least "$cpu" "$SHED_CPU_THRESHOLD"; then
      log_line "action=shed_container container=$part container_cpu_pct=$cpu shed_action=$SHED_ACTION"
      container_control "$part" || true
      changed=1
    fi
  done
  [[ "$changed" == "1" ]]
}

find_gateway_container() {
  local output
  output="$(capture_timeout "$DOCKER_TIMEOUT_SECS" docker --context "$DOCKER_CONTEXT_NAME" ps \
    --filter "label=com.docker.compose.service=${ORCH_SERVICE_LABEL}" --format '{{.Names}}')" || return 1
  printf '%s\n' "$output" | awk 'NF {print; exit}'
}

FORWARD_ACTION="none"
repair_forward() {
  local now last container outage_attempted
  if [[ "$ALLOW_FORWARD_REPAIR" != "1" ]]; then
    FORWARD_ACTION="health_unavailable_no_repair"
    log_line "action=$FORWARD_ACTION result=observed"
    return 1
  fi
  now="$(date +%s)"
  last="$(state_uint last_forward_repair_ts)"
  outage_attempted="$(state_uint forward_repair_attempted_for_outage)"
  if [[ "$outage_attempted" == "1" ]]; then
    FORWARD_ACTION="forward_repair_suppressed_outage_latch"
    log_line "action=$FORWARD_ACTION"
    return 1
  fi
  if (( last > 0 && now - last < FORWARD_REPAIR_COOLDOWN_SECS )); then
    FORWARD_ACTION="forward_repair_suppressed_cooldown"
    log_line "action=$FORWARD_ACTION"
    return 1
  fi
  container="$(find_gateway_container || true)"
  if [[ -z "$container" ]]; then
    FORWARD_ACTION="forward_repair_unavailable"
    log_line "action=$FORWARD_ACTION reason=gateway_container_missing"
    return 1
  fi
  write_state last_forward_repair_ts "$now"
  write_state forward_repair_attempted_for_outage 1
  FORWARD_ACTION="forward_repair"
  log_line "action=$FORWARD_ACTION container=$container begin"
  if run_timeout "$DOCKER_TIMEOUT_SECS" docker --context "$DOCKER_CONTEXT_NAME" restart "$container"; then
    log_line "action=$FORWARD_ACTION container=$container result=restarted"
    return 0
  fi
  FORWARD_ACTION="forward_repair_failed"
  log_line "action=$FORWARD_ACTION container=$container"
  return 1
}

orbstack_transitioning() {
  local output lowered
  command -v orb >/dev/null 2>&1 || return 1
  output="$(capture_timeout "$DOCKER_TIMEOUT_SECS" orb status)" || return 1
  lowered="$(printf '%s' "$output" | tr '[:upper:]' '[:lower:]')"
  [[ "$lowered" == *starting* || "$lowered" == *stopping* ]]
}

restart_window_refresh() {
  local now="$1" started count
  started="$(state_uint restart_window_started_ts)"
  count="$(state_uint restart_window_count)"
  if (( started == 0 || now - started >= RESTART_WINDOW_SECS )); then
    started="$now"
    count=0
    write_state restart_window_started_ts "$started"
    write_state restart_window_count "$count"
  fi
  RESTART_WINDOW_COUNT="$count"
}

RESTART_ACTION="none"
RESTART_WINDOW_COUNT=0
attempt_vm_restart() {
  local now started last_restart attempt outage_attempted
  now="$(date +%s)"
  started="$(state_uint supervisor_started_ts "$now")"
  last_restart="$(state_uint last_restart_ts)"
  outage_attempted="$(state_uint restart_attempted_for_outage)"

  if [[ "$ALLOW_VM_RESTART" != "1" ]]; then
    RESTART_ACTION="docker_unavailable_no_restart"
    return 1
  fi
  if [[ "$outage_attempted" == "1" ]]; then
    RESTART_ACTION="restart_suppressed_outage_latch"
    log_line "action=$RESTART_ACTION"
    return 1
  fi
  if (( now - started < STARTUP_GRACE_SECS )); then
    RESTART_ACTION="restart_suppressed_startup_grace"
    log_line "action=$RESTART_ACTION"
    return 1
  fi
  if orbstack_transitioning; then
    RESTART_ACTION="restart_suppressed_orbstack_transition"
    log_line "action=$RESTART_ACTION"
    return 1
  fi
  if (( last_restart > 0 && now - last_restart < POST_RESTART_GRACE_SECS )); then
    RESTART_ACTION="restart_suppressed_post_restart_grace"
    log_line "action=$RESTART_ACTION"
    return 1
  fi
  if (( last_restart > 0 && now - last_restart < RESTART_COOLDOWN_SECS )); then
    RESTART_ACTION="restart_suppressed_cooldown"
    log_line "action=$RESTART_ACTION"
    return 1
  fi

  restart_window_refresh "$now"
  if (( RESTART_WINDOW_COUNT >= MAX_RESTARTS_PER_WINDOW )); then
    RESTART_ACTION="restart_suppressed_window_limit"
    log_line "action=$RESTART_ACTION window_count=$RESTART_WINDOW_COUNT"
    return 1
  fi
  if ! command -v orb >/dev/null 2>&1; then
    RESTART_ACTION="restart_unavailable_orb_missing"
    log_line "action=$RESTART_ACTION"
    return 1
  fi

  RESTART_WINDOW_COUNT=$((RESTART_WINDOW_COUNT + 1))
  write_state restart_window_count "$RESTART_WINDOW_COUNT"
  write_state last_restart_ts "$now"
  write_state restart_attempted_for_outage 1
  RESTART_ACTION="restart_orbstack"
  log_line "action=$RESTART_ACTION begin window_count=$RESTART_WINDOW_COUNT"

  if ! run_timeout 20 orb stop; then
    RESTART_ACTION="restart_aborted_graceful_stop_failed"
    write_state last_restart_result "graceful_stop_failed"
    log_line "action=$RESTART_ACTION force_stop=false"
    return 1
  fi
  if ! run_timeout 30 orb start; then
    RESTART_ACTION="restart_start_failed"
    write_state last_restart_result "start_failed"
    log_line "action=$RESTART_ACTION"
    return 1
  fi

  attempt=1
  while (( attempt <= RESTART_READY_ATTEMPTS )); do
    if docker_server_ok; then
      write_state consecutive_docker_failures 0
      write_state last_restart_result "ready"
      log_line "action=restart_orbstack docker_server=ok attempt=$attempt"
      RESTART_ACTION="restart_orbstack"
      return 0
    fi
    sleep "$RESTART_READY_INTERVAL_SECS"
    attempt=$((attempt + 1))
  done
  RESTART_ACTION="restart_docker_not_ready"
  write_state last_restart_result "docker_not_ready"
  log_line "action=$RESTART_ACTION attempts=$RESTART_READY_ATTEMPTS"
  return 1
}

run_once() {
  local now docker_ok="false" health="false" cpu action="none" failures health_failures
  if ! acquire_lock; then
    json_emit ok=true action=skipped_locked event="$EVENT" vm_restart_enabled="$([[ "$ALLOW_VM_RESTART" == "1" ]] && echo true || echo false)"
    return 0
  fi
  trap release_lock EXIT INT TERM

  now="$(date +%s)"
  if [[ "$(state_uint supervisor_started_ts)" == "0" ]]; then
    write_state supervisor_started_ts "$now"
  fi
  cpu="$(orbstack_cpu_pct)"

  if docker_server_ok; then
    docker_ok="true"
    write_state consecutive_docker_failures 0
  fi

  if [[ "$docker_ok" == "true" ]] && health_ok; then
    health="true"
    write_state last_ok_ts "$now"
    write_state consecutive_health_failures 0
    write_state forward_repair_attempted_for_outage 0
    write_state restart_attempted_for_outage 0
    if shed_high_cpu_containers; then
      action="shed_container"
      log_line "action=$action result=applied cpu_pct=$cpu"
    elif [[ "$cpu" != "0.0" ]] && number_at_least "$cpu" "$SHED_CPU_THRESHOLD"; then
      log_line "action=none observation=orbstack_high_cpu cpu_pct=$cpu vm_restart=false"
    fi
    json_emit ok=true action="$action" docker_server=true health=true cpu_pct="$cpu" event="$EVENT" \
      vm_restart_enabled="$([[ "$ALLOW_VM_RESTART" == "1" ]] && echo true || echo false)" force_stop_enabled=false
    return 0
  fi

  if [[ "$docker_ok" == "true" ]]; then
    health_failures="$(state_uint consecutive_health_failures)"
    health_failures=$((health_failures + 1))
    write_state consecutive_health_failures "$health_failures"
    if (( health_failures < HEALTH_FAILURES_BEFORE_REPAIR )); then
      action="health_failure_threshold_wait"
      log_line "action=$action consecutive_failures=$health_failures required=$HEALTH_FAILURES_BEFORE_REPAIR"
    else
      repair_forward || true
      action="$FORWARD_ACTION"
    fi
  else
    failures="$(state_uint consecutive_docker_failures)"
    failures=$((failures + 1))
    write_state consecutive_docker_failures "$failures"
    if [[ "$ALLOW_VM_RESTART" != "1" ]]; then
      action="docker_unavailable_no_restart"
      log_line "action=$action result=observed consecutive_failures=$failures"
    elif (( failures < FAILURES_BEFORE_RESTART )); then
      action="docker_failure_threshold_wait"
      log_line "action=$action consecutive_failures=$failures required=$FAILURES_BEFORE_RESTART"
    else
      attempt_vm_restart || true
      action="$RESTART_ACTION"
    fi
  fi

  docker_ok="false"
  health="false"
  if docker_server_ok; then
    docker_ok="true"
    write_state consecutive_docker_failures 0
    if health_ok; then
      health="true"
      write_state last_ok_ts "$(date +%s)"
    fi
  fi
  if [[ "$docker_ok" == "true" && "$health" == "true" ]]; then
    write_state consecutive_health_failures 0
    write_state forward_repair_attempted_for_outage 0
    write_state restart_attempted_for_outage 0
    log_line "action=$action result=ok cpu_pct=$cpu"
    json_emit ok=true action="$action" docker_server=true health=true cpu_pct="$cpu" event="$EVENT" \
      vm_restart_enabled="$([[ "$ALLOW_VM_RESTART" == "1" ]] && echo true || echo false)" force_stop_enabled=false
    return 0
  fi

  log_line "action=$action result=failed docker_server=$docker_ok health=$health cpu_pct=$cpu"
  json_emit ok=false action="$action" docker_server="$docker_ok" health="$health" cpu_pct="$cpu" event="$EVENT" \
    vm_restart_enabled="$([[ "$ALLOW_VM_RESTART" == "1" ]] && echo true || echo false)" force_stop_enabled=false
  return 1
}

status() {
  local cpu loaded="false" shedding="false"
  cpu="$(orbstack_cpu_pct)"
  if [[ "$(uname -s)" == "Darwin" ]] && command -v launchctl >/dev/null 2>&1; then
    if launchctl print "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1; then
      loaded="true"
    fi
  fi
  [[ -n "$SHED_SERVICES" ]] && shedding="true"
  json_emit ok=true launchd_loaded="$loaded" cpu_pct="$cpu" state_file="$STATE_FILE" log_file="$LOG_FILE" \
    docker_context="$DOCKER_CONTEXT_NAME" forward_repair_enabled="$([[ "$ALLOW_FORWARD_REPAIR" == "1" ]] && echo true || echo false)" \
    vm_restart_enabled="$([[ "$ALLOW_VM_RESTART" == "1" ]] && echo true || echo false)" force_stop_enabled=false kill_enabled=false \
    container_shedding_enabled="$shedding" consecutive_docker_failures="$(state_uint consecutive_docker_failures)" \
    consecutive_health_failures="$(state_uint consecutive_health_failures)" health_failures_before_repair="$HEALTH_FAILURES_BEFORE_REPAIR" \
    forward_repair_attempted_for_outage="$(state_uint forward_repair_attempted_for_outage)" \
    failures_before_restart="$FAILURES_BEFORE_RESTART" restart_cooldown_secs="$RESTART_COOLDOWN_SECS" \
    restart_window_secs="$RESTART_WINDOW_SECS" max_restarts_per_window="$MAX_RESTARTS_PER_WINDOW" \
    restart_window_count="$(state_uint restart_window_count)" restart_attempted_for_outage="$(state_uint restart_attempted_for_outage)"
}

xml_escape() {
  local value="$1"
  value="${value//&/&amp;}"
  value="${value//</&lt;}"
  value="${value//>/&gt;}"
  value="${value//\"/&quot;}"
  value="${value//\'/&apos;}"
  printf '%s' "$value"
}

install_atomic() {
  local source="$1" destination="$2" tmp
  tmp="${destination}.tmp.$$"
  install -m 0755 "$source" "$tmp"
  mv "$tmp" "$destination"
}

start() {
  local launch_root launch_scripts launch_script launch_ensure launch_runtime launch_log current_script
  local plist_label plist_script plist_runtime plist_log plist_context plist_health plist_shed plist_service
  launch_root="${CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCH_ROOT:-${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}}"
  launch_scripts="${launch_root}/scripts"
  launch_script="${launch_scripts}/orbstack_self_heal.sh"
  launch_ensure="${launch_scripts}/ensure_docker_runtime.sh"
  launch_runtime="${CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCH_RUNTIME_DIR:-${launch_root}/.data/runtime/orbstack-self-heal}"
  launch_log="${launch_runtime}/launchd.log"
  current_script="${ROOT_DIR}/scripts/orbstack_self_heal.sh"

  if [[ "$(uname -s)" != "Darwin" ]] || ! command -v launchctl >/dev/null 2>&1; then
    echo "OrbStack launchd supervision is available only on macOS." >&2
    return 1
  fi

  mkdir -p "$(dirname "$LAUNCHD_PLIST")" "$launch_scripts" "$launch_runtime"
  if [[ "$current_script" != "$launch_script" ]]; then
    install_atomic "$current_script" "$launch_script"
  fi
  if [[ "${ROOT_DIR}/scripts/ensure_docker_runtime.sh" != "$launch_ensure" ]]; then
    install_atomic "${ROOT_DIR}/scripts/ensure_docker_runtime.sh" "$launch_ensure"
  fi

  plist_label="$(xml_escape "$LAUNCHD_LABEL")"
  plist_script="$(xml_escape "$launch_script")"
  plist_runtime="$(xml_escape "$launch_runtime")"
  plist_log="$(xml_escape "$launch_log")"
  plist_context="$(xml_escape "$DOCKER_CONTEXT_NAME")"
  plist_health="$(xml_escape "$HEALTH_URL")"
  plist_shed="$(xml_escape "$SHED_SERVICES")"
  plist_service="$(xml_escape "$ORCH_SERVICE_LABEL")"
  cat > "$LAUNCHD_PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${plist_label}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>${plist_script}</string>
    <string>run-once</string>
    <string>--event</string>
    <string>launchd</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>CONTEXTLATTICE_DOCKER_RUNTIME</key>
    <string>orbstack</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_RUNTIME_DIR</key>
    <string>${plist_runtime}</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART</key>
    <string>${ALLOW_VM_RESTART}</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_FORWARD_REPAIR</key>
    <string>${ALLOW_FORWARD_REPAIR}</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_HEALTH_FAILURES_BEFORE_REPAIR</key>
    <string>${HEALTH_FAILURES_BEFORE_REPAIR}</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_SHED_SERVICES</key>
    <string>${plist_shed}</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_FAILURES_BEFORE_RESTART</key>
    <string>${FAILURES_BEFORE_RESTART}</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_STARTUP_GRACE_SECS</key>
    <string>${STARTUP_GRACE_SECS}</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_POST_RESTART_GRACE_SECS</key>
    <string>${POST_RESTART_GRACE_SECS}</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_COOLDOWN_SECS</key>
    <string>${RESTART_COOLDOWN_SECS}</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_RESTART_WINDOW_SECS</key>
    <string>${RESTART_WINDOW_SECS}</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_MAX_RESTARTS_PER_WINDOW</key>
    <string>${MAX_RESTARTS_PER_WINDOW}</string>
    <key>CONTEXTLATTICE_HEAL_ORCH_URL</key>
    <string>${plist_health}</string>
    <key>CONTEXTLATTICE_HEAL_ORCH_SERVICE_LABEL</key>
    <string>${plist_service}</string>
    <key>DOCKER_CONTEXT</key>
    <string>${plist_context}</string>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>StartInterval</key>
  <integer>${INTERVAL_SECS}</integer>
  <key>StandardOutPath</key>
  <string>${plist_log}</string>
  <key>StandardErrorPath</key>
  <string>${plist_log}</string>
</dict>
</plist>
EOF
  launchctl bootout "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/${UID}" "$LAUNCHD_PLIST"
  launchctl enable "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
  launchctl kickstart -k "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
  log_line "action=start vm_restart_enabled=$ALLOW_VM_RESTART force_stop=false docker_context=$DOCKER_CONTEXT_NAME"
  json_emit ok=true action=start launchd_label="$LAUNCHD_LABEL" plist="$LAUNCHD_PLIST" launch_script="$launch_script" \
    runtime_dir="$launch_runtime" vm_restart_enabled="$([[ "$ALLOW_VM_RESTART" == "1" ]] && echo true || echo false)" force_stop_enabled=false
}

stop() {
  if [[ "$(uname -s)" == "Darwin" ]]; then
    if ! command -v launchctl >/dev/null 2>&1; then
      json_emit ok=false action=stop launchd_label="$LAUNCHD_LABEL" error=launchctl_missing
      return 1
    fi
    if launchctl print "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1; then
      if ! launchctl bootout "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1; then
        json_emit ok=false action=stop launchd_label="$LAUNCHD_LABEL" error=bootout_failed plist_retained=true
        return 1
      fi
      if launchctl print "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1; then
        json_emit ok=false action=stop launchd_label="$LAUNCHD_LABEL" error=still_loaded plist_retained=true
        return 1
      fi
    fi
  fi
  rm -f "$LAUNCHD_PLIST"
  json_emit ok=true action=stop launchd_label="$LAUNCHD_LABEL"
}

case "$CMD" in
  run-once) run_once ;;
  start) start ;;
  stop) stop ;;
  status) status ;;
esac
