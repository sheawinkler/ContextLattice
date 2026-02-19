#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

mkdir -p logs tmp "$HOME/Library/LaunchAgents"

ACTION="${1:-schedule}"
LOAD_RATE="${LOAD_RATE:-12}"
LOAD_SECONDS="${LOAD_SECONDS:-7200}"
LOAD_THREADS="${LOAD_THREADS:-24}"
DRAIN_TIMEOUT_SECS="${DRAIN_TIMEOUT_SECS:-1800}"
LABEL="com.sheawinkler.memmcp.launchgate0430mt"
PLIST_PATH="$HOME/Library/LaunchAgents/${LABEL}.plist"
META_FILE="${ROOT_DIR}/tmp/launch_gate_0430MT.meta"
RUNNER_SCRIPT="${ROOT_DIR}/tmp/launch_gate_0430MT.runner.sh"
USER_ID="$(id -u)"

read -r TARGET_YEAR TARGET_MONTH TARGET_DAY TARGET_ISO TARGET_TAG < <(
  python3 - <<'PY'
from datetime import datetime, timedelta
from zoneinfo import ZoneInfo

tz = ZoneInfo("America/Denver")
now = datetime.now(tz)
target = now.replace(hour=4, minute=30, second=0, microsecond=0)
if target <= now:
    target += timedelta(days=1)
print(target.year, target.month, target.day, target.isoformat(), target.strftime("%Y%m%d"))
PY
)

LOG_FILE="${ROOT_DIR}/logs/launch_gate_${TARGET_TAG}_0430MT.log"
ERROR_LOG_FILE="${ROOT_DIR}/logs/launch_gate_${TARGET_TAG}_0430MT.err.log"

print_status() {
  if [[ -f "$META_FILE" ]]; then
    cat "$META_FILE"
  else
    echo "No scheduled launch gate metadata found."
  fi
  if launchctl print "gui/${USER_ID}/${LABEL}" >/dev/null 2>&1; then
    echo "status=scheduled"
    return 0
  fi
  echo "status=none"
  return 1
}

cancel_schedule() {
  launchctl bootout "gui/${USER_ID}" "$PLIST_PATH" >/dev/null 2>&1 || true
  launchctl disable "gui/${USER_ID}/${LABEL}" >/dev/null 2>&1 || true
  rm -f "$PLIST_PATH" "$RUNNER_SCRIPT" "$META_FILE"
  echo "Cancelled launch gate schedule."
}

schedule_gate() {
  cat >"$RUNNER_SCRIPT" <<EOF
#!/usr/bin/env bash
set -euo pipefail
cd "$ROOT_DIR"
LOAD_RATE="$LOAD_RATE" LOAD_SECONDS="$LOAD_SECONDS" LOAD_THREADS="$LOAD_THREADS" DRAIN_TIMEOUT_SECS="$DRAIN_TIMEOUT_SECS" \
  scripts/launch_readiness_gate.sh
launchctl bootout "gui/${USER_ID}" "$PLIST_PATH" >/dev/null 2>&1 || true
launchctl disable "gui/${USER_ID}/${LABEL}" >/dev/null 2>&1 || true
rm -f "$PLIST_PATH"
echo "completed_at_iso=\$(date -u +\"%Y-%m-%dT%H:%M:%SZ\")" >> "$META_FILE"
EOF
  chmod +x "$RUNNER_SCRIPT"

  cat >"$PLIST_PATH" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${RUNNER_SCRIPT}</string>
  </array>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Month</key>
    <integer>${TARGET_MONTH}</integer>
    <key>Day</key>
    <integer>${TARGET_DAY}</integer>
    <key>Hour</key>
    <integer>4</integer>
    <key>Minute</key>
    <integer>30</integer>
  </dict>
  <key>RunAtLoad</key>
  <false/>
  <key>StandardOutPath</key>
  <string>${LOG_FILE}</string>
  <key>StandardErrorPath</key>
  <string>${ERROR_LOG_FILE}</string>
</dict>
</plist>
PLIST

  launchctl bootout "gui/${USER_ID}" "$PLIST_PATH" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/${USER_ID}" "$PLIST_PATH"
  launchctl enable "gui/${USER_ID}/${LABEL}"

  cat >"$META_FILE" <<EOF
scheduled_at_iso=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
target_mt_iso=$TARGET_ISO
target_mt_year=$TARGET_YEAR
target_mt_month=$TARGET_MONTH
target_mt_day=$TARGET_DAY
load_rate=$LOAD_RATE
load_seconds=$LOAD_SECONDS
load_threads=$LOAD_THREADS
drain_timeout_secs=$DRAIN_TIMEOUT_SECS
runner_script=$RUNNER_SCRIPT
plist_path=$PLIST_PATH
log_file=$LOG_FILE
error_log_file=$ERROR_LOG_FILE
EOF

  echo "Scheduled launch gate for 04:30 America/Denver."
  echo "target_mt_iso=$TARGET_ISO"
  echo "plist_path=$PLIST_PATH"
  echo "log_file=$LOG_FILE"
}

case "$ACTION" in
  schedule)
    schedule_gate
    ;;
  status)
    print_status
    ;;
  cancel|stop)
    cancel_schedule
    ;;
  *)
    echo "Usage: $0 [schedule|status|cancel]" >&2
    exit 2
    ;;
esac
