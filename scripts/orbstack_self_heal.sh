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
LAUNCHD_LABEL="${CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCHD_LABEL:-io.contextlattice.orbstack-self-heal}"
LAUNCHD_PLIST="${CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCHD_PLIST:-$HOME/Library/LaunchAgents/${LAUNCHD_LABEL}.plist}"
INTERVAL_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_INTERVAL_SECS:-30}"
COOLDOWN_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_COOLDOWN_SECS:-120}"
DOCKER_TIMEOUT_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_DOCKER_TIMEOUT_SECS:-5}"
HEALTH_TIMEOUT_SECS="${CONTEXTLATTICE_ORBSTACK_HEAL_HEALTH_TIMEOUT_SECS:-3}"
HEALTH_URL="${CONTEXTLATTICE_HEAL_ORCH_URL:-http://127.0.0.1:8075/health}"
CPU_RESTART_THRESHOLD="${CONTEXTLATTICE_ORBSTACK_HEAL_CPU_THRESHOLD:-250}"
SHED_CPU_THRESHOLD="${CONTEXTLATTICE_ORBSTACK_HEAL_CONTAINER_CPU_THRESHOLD:-800}"
SHED_SERVICES="${CONTEXTLATTICE_ORBSTACK_HEAL_SHED_SERVICES:-ollama}"
SHED_ACTION="${CONTEXTLATTICE_ORBSTACK_HEAL_SHED_ACTION:-restart}"
ALLOW_FORCE_STOP="${CONTEXTLATTICE_ORBSTACK_HEAL_FORCE_STOP:-1}"
ALLOW_KILL="${CONTEXTLATTICE_ORBSTACK_HEAL_KILL:-0}"
ALLOW_VM_RESTART="${CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART:-0}"

mkdir -p "$RUNTIME_DIR"

log_line() {
  printf '%s event=%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$EVENT" "$*" >> "$LOG_FILE"
}

read_state() {
  local key="$1"
  if [[ -f "$STATE_FILE" ]]; then
    awk -F= -v k="$key" '$1==k {print substr($0, index($0, "=")+1)}' "$STATE_FILE" | tail -1
  fi
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

json_emit() {
  python3 - "$@" <<'PY'
import json, sys
out = {}
for pair in sys.argv[1:]:
    if "=" not in pair:
        continue
    key, value = pair.split("=", 1)
    if value in ("true", "false"):
        out[key] = value == "true"
    else:
        try:
            out[key] = int(value)
        except ValueError:
            try:
                out[key] = float(value)
            except ValueError:
                out[key] = value
print(json.dumps(out, separators=(",", ":")))
PY
}

run_timeout() {
  local timeout="$1"
  shift
  python3 - "$timeout" "$@" <<'PY'
import subprocess, sys
timeout = float(sys.argv[1])
cmd = sys.argv[2:]
try:
    proc = subprocess.run(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=timeout, check=False)
except Exception:
    raise SystemExit(124)
raise SystemExit(proc.returncode)
PY
}

docker_server_ok() {
  run_timeout "$DOCKER_TIMEOUT_SECS" docker version --format '{{.Server.Version}}'
}

health_ok() {
  python3 - "$HEALTH_TIMEOUT_SECS" "$HEALTH_URL" <<'PY'
import sys, urllib.request
timeout = float(sys.argv[1])
url = sys.argv[2]
try:
    with urllib.request.urlopen(url, timeout=timeout) as resp:
        body = resp.read(4096).decode("utf-8", errors="replace")
except Exception:
    raise SystemExit(1)
raise SystemExit(0 if '"ok":true' in body else 1)
PY
}

orbstack_cpu_pct() {
  ps -axo %cpu,command | awk '/OrbStack Helper.*vmgr/ {sum += $1} END {printf "%.1f\n", sum + 0}'
}

container_cpu_pct() {
  local container="$1"
  docker stats --no-stream --format '{{.Name}}	{{.CPUPerc}}' 2>/dev/null | awk -v wanted="$container" '
    $1 == wanted {
      gsub(/%/, "", $2)
      print $2
      found=1
    }
    END {
      if (!found) print "0"
    }
  '
}

container_exists() {
  local container="$1"
  run_timeout "$DOCKER_TIMEOUT_SECS" docker inspect "$container"
}

container_control() {
  local container="$1"
  case "$SHED_ACTION" in
    stop)
      run_timeout "$DOCKER_TIMEOUT_SECS" docker stop "$container"
      ;;
    restart|*)
      run_timeout "$DOCKER_TIMEOUT_SECS" docker restart "$container"
      ;;
  esac
}

shed_high_cpu_containers() {
  local changed=0 raw part cpu
  raw="${SHED_SERVICES//,/ }"
  for part in $raw; do
    [[ -n "$part" ]] || continue
    container_exists "$part" || continue
    cpu="$(container_cpu_pct "$part")"
    if awk "BEGIN {exit !($cpu >= $SHED_CPU_THRESHOLD)}"; then
      log_line "action=shed_container container=$part container_cpu_pct=$cpu shed_action=$SHED_ACTION"
      container_control "$part" || true
      changed=1
    fi
  done
  [[ "$changed" == "1" ]]
}

cooldown_active() {
  local last now
  last="$(read_state last_restart_ts)"
  [[ -n "$last" ]] || return 1
  now="$(date +%s)"
  (( now - last < COOLDOWN_SECS ))
}

restart_orbstack() {
  local now
  if cooldown_active; then
    log_line "action=restart skipped reason=cooldown"
    return 1
  fi
  now="$(date +%s)"
  write_state last_restart_ts "$now"
  log_line "action=restart begin"
  if command -v orb >/dev/null 2>&1; then
    run_timeout 20 orb stop || {
      if [[ "$ALLOW_FORCE_STOP" == "1" ]]; then
        log_line "action=restart graceful_stop_failed trying_force_stop=true"
        run_timeout 15 orb stop --force || true
      fi
    }
  fi
  if [[ "$ALLOW_KILL" == "1" ]]; then
    pkill -f 'OrbStack Helper.*vmgr' >/dev/null 2>&1 || true
  fi
  if [[ "$(uname -s)" == "Darwin" ]]; then
    open -gja OrbStack >/dev/null 2>&1 || true
  fi
  if command -v orb >/dev/null 2>&1; then
    run_timeout 30 orb start || true
  fi
  for _ in 1 2 3 4 5 6; do
    if docker_server_ok; then
      log_line "action=restart docker_server=ok"
      return 0
    fi
    sleep 2
  done
  log_line "action=restart docker_server=failed"
  return 1
}

run_once() {
  local docker_ok="false" health="false" cpu action="none"
  cpu="$(orbstack_cpu_pct)"
  if docker_server_ok; then
    docker_ok="true"
  fi
  if health_ok; then
    health="true"
  fi

  if [[ "$docker_ok" == "true" && "$health" == "true" ]]; then
    if shed_high_cpu_containers; then
      log_line "action=shed_container result=applied cpu_pct=$cpu"
      json_emit ok=true action=shed_container docker_server=true health=true cpu_pct="$cpu" event="$EVENT"
      return 0
    fi
    write_state last_ok_ts "$(date +%s)"
    json_emit ok=true action=none docker_server=true health=true cpu_pct="$cpu" event="$EVENT"
    return 0
  fi

  if [[ "$docker_ok" != "true" ]]; then
    if [[ "$ALLOW_VM_RESTART" == "1" ]]; then
      action="restart_orbstack"
      restart_orbstack || true
    else
      action="docker_unavailable_no_restart"
      log_line "action=$action result=observed"
    fi
  elif awk "BEGIN {exit !($cpu >= $CPU_RESTART_THRESHOLD)}"; then
    action="restart_orbstack_high_cpu"
    restart_orbstack || true
  else
    action="repair_forward"
    DOCKER_PROBE_TIMEOUT_SECS="$DOCKER_TIMEOUT_SECS" \
      DOCKER_CMD_TIMEOUT_SECS="$DOCKER_TIMEOUT_SECS" \
      CONTEXTLATTICE_HEAL_ORCH_TIMEOUT_SECS="$HEALTH_TIMEOUT_SECS" \
      CONTEXTLATTICE_DOCKER_RUNTIME=orbstack \
      scripts/ensure_docker_runtime.sh >/dev/null 2>&1 || true
  fi

  docker_ok="false"
  health="false"
  if docker_server_ok; then docker_ok="true"; fi
  if health_ok; then health="true"; fi
  if [[ "$docker_ok" == "true" && "$health" == "true" ]]; then
    write_state last_ok_ts "$(date +%s)"
    log_line "action=$action result=ok cpu_pct=$cpu"
    json_emit ok=true action="$action" docker_server=true health=true cpu_pct="$cpu" event="$EVENT"
    return 0
  fi

  log_line "action=$action result=failed docker_server=$docker_ok health=$health cpu_pct=$cpu"
  json_emit ok=false action="$action" docker_server="$docker_ok" health="$health" cpu_pct="$cpu" event="$EVENT"
  return 1
}

status() {
  local cpu loaded
  cpu="$(orbstack_cpu_pct)"
  loaded="false"
  if [[ "$(uname -s)" == "Darwin" ]] && command -v launchctl >/dev/null 2>&1; then
    if launchctl print "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1; then
      loaded="true"
    fi
  fi
  json_emit ok=true launchd_loaded="$loaded" cpu_pct="$cpu" state_file="$STATE_FILE" log_file="$LOG_FILE"
}

start() {
  local launch_root launch_scripts launch_script launch_ensure launch_runtime launch_log current_script
  launch_root="${CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCH_ROOT:-${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}}"
  launch_scripts="${launch_root}/scripts"
  launch_script="${launch_scripts}/orbstack_self_heal.sh"
  launch_ensure="${launch_scripts}/ensure_docker_runtime.sh"
  launch_runtime="${CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCH_RUNTIME_DIR:-${launch_root}/.data/runtime/orbstack-self-heal}"
  launch_log="${launch_runtime}/launchd.log"
  current_script="${ROOT_DIR}/scripts/orbstack_self_heal.sh"

  mkdir -p "$(dirname "$LAUNCHD_PLIST")" "$launch_scripts" "$launch_runtime"
  if [[ "$current_script" != "$launch_script" ]]; then
    install -m 0755 "$current_script" "$launch_script"
  fi
  if [[ "${ROOT_DIR}/scripts/ensure_docker_runtime.sh" != "$launch_ensure" ]]; then
    install -m 0755 "${ROOT_DIR}/scripts/ensure_docker_runtime.sh" "$launch_ensure"
  fi
  cat > "$LAUNCHD_PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LAUNCHD_LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>${launch_script}</string>
    <string>run-once</string>
    <string>--event</string>
    <string>launchd</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>CONTEXTLATTICE_DOCKER_RUNTIME</key>
    <string>orbstack</string>
    <key>CONTEXTLATTICE_ORBSTACK_HEAL_RUNTIME_DIR</key>
    <string>${launch_runtime}</string>
    <key>DOCKER_CONTEXT</key>
    <string>orbstack</string>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>StartInterval</key>
  <integer>${INTERVAL_SECS}</integer>
  <key>StandardOutPath</key>
  <string>${launch_log}</string>
  <key>StandardErrorPath</key>
  <string>${launch_log}</string>
</dict>
</plist>
EOF
  launchctl bootout "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/${UID}" "$LAUNCHD_PLIST"
  launchctl enable "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
  launchctl kickstart -k "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
  json_emit ok=true action=start launchd_label="$LAUNCHD_LABEL" plist="$LAUNCHD_PLIST" launch_script="$launch_script" runtime_dir="$launch_runtime"
}

stop() {
  launchctl bootout "gui/${UID}/${LAUNCHD_LABEL}" >/dev/null 2>&1 || true
  rm -f "$LAUNCHD_PLIST"
  json_emit ok=true action=stop launchd_label="$LAUNCHD_LABEL"
}

case "$CMD" in
  run-once) run_once ;;
  start) start ;;
  stop) stop ;;
  status) status ;;
esac
