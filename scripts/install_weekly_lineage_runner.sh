#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LABEL="com.sheawinkler.contextlattice-weekly-lineage"
PLIST_PATH="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_DIR="$REPO_ROOT/logs"
WEEKDAY="${LINEAGE_WEEKLY_WEEKDAY:-1}"   # 1 = Monday
HOUR="${LINEAGE_WEEKLY_HOUR:-5}"
MINUTE="${LINEAGE_WEEKLY_MINUTE:-15}"
RUN_AT_LOAD="${LINEAGE_WEEKLY_RUN_AT_LOAD:-0}"
USER_ID="$(id -u)"
ACTION="${1:-install}"

for value in "$WEEKDAY" "$HOUR" "$MINUTE"; do
  if ! [[ "$value" =~ ^[0-9]+$ ]]; then
    echo "weekday/hour/minute must be integers" >&2
    exit 2
  fi
done

if [[ "$WEEKDAY" -lt 0 || "$WEEKDAY" -gt 7 ]]; then
  echo "LINEAGE_WEEKLY_WEEKDAY must be 0..7 (1=Monday)" >&2
  exit 2
fi
if [[ "$HOUR" -lt 0 || "$HOUR" -gt 23 ]]; then
  echo "LINEAGE_WEEKLY_HOUR must be 0..23" >&2
  exit 2
fi
if [[ "$MINUTE" -lt 0 || "$MINUTE" -gt 59 ]]; then
  echo "LINEAGE_WEEKLY_MINUTE must be 0..59" >&2
  exit 2
fi

if [[ "$RUN_AT_LOAD" == "1" ]]; then
  RUN_AT_LOAD_XML="<true/>"
else
  RUN_AT_LOAD_XML="<false/>"
fi

install_agent() {
  mkdir -p "$HOME/Library/LaunchAgents" "$LOG_DIR"

  cat > "$PLIST_PATH" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${REPO_ROOT}/scripts/weekly_lineage_runner.sh</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${REPO_ROOT}</string>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Weekday</key>
    <integer>${WEEKDAY}</integer>
    <key>Hour</key>
    <integer>${HOUR}</integer>
    <key>Minute</key>
    <integer>${MINUTE}</integer>
  </dict>
  <key>RunAtLoad</key>
  ${RUN_AT_LOAD_XML}
  <key>StandardOutPath</key>
  <string>${LOG_DIR}/weekly-lineage-runner.log</string>
  <key>StandardErrorPath</key>
  <string>${LOG_DIR}/weekly-lineage-runner.err</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${PATH}</string>
  </dict>
</dict>
</plist>
PLIST

  launchctl bootout "gui/${USER_ID}" "$PLIST_PATH" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/${USER_ID}" "$PLIST_PATH"
  launchctl enable "gui/${USER_ID}/${LABEL}"
  if [[ "$RUN_AT_LOAD" == "1" ]]; then
    launchctl kickstart -k "gui/${USER_ID}/${LABEL}"
  fi
  echo "Installed LaunchAgent: ${PLIST_PATH}"
}

uninstall_agent() {
  launchctl bootout "gui/${USER_ID}" "$PLIST_PATH" >/dev/null 2>&1 || true
  rm -f "$PLIST_PATH"
  echo "Removed LaunchAgent: ${PLIST_PATH}"
}

status_agent() {
  if launchctl print "gui/${USER_ID}/${LABEL}" >/dev/null 2>&1; then
    launchctl print "gui/${USER_ID}/${LABEL}"
    return
  fi
  if launchctl list | grep -q "${LABEL}"; then
    launchctl list | grep "${LABEL}"
    return
  fi
  echo "LaunchAgent not loaded: ${LABEL}"
}

case "$ACTION" in
  install)
    install_agent
    ;;
  uninstall|remove)
    uninstall_agent
    ;;
  status)
    status_agent
    ;;
  *)
    echo "Usage: $0 [install|uninstall|status]" >&2
    exit 2
    ;;
esac
