#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LABEL="com.sheawinkler.memmcp-retention"
PLIST_PATH="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_DIR="$REPO_ROOT/logs"
INTERVAL_SECONDS="${RETENTION_INTERVAL_SECONDS:-2100}"
RUN_AT_LOAD="${RETENTION_RUN_AT_LOAD:-0}"
DOCKER_API_VERSION_VALUE="${DOCKER_API_VERSION:-}"
USER_ID="$(id -u)"
ACTION="${1:-install}"

if ! [[ "$INTERVAL_SECONDS" =~ ^[0-9]+$ ]] || [[ "$INTERVAL_SECONDS" -le 0 ]]; then
  echo "RETENTION_INTERVAL_SECONDS must be a positive integer (got: $INTERVAL_SECONDS)" >&2
  exit 2
fi

if [[ "$RUN_AT_LOAD" == "1" ]]; then
  RUN_AT_LOAD_XML="<true/>"
else
  RUN_AT_LOAD_XML="<false/>"
fi

if [[ -n "$DOCKER_API_VERSION_VALUE" ]]; then
  DOCKER_API_VERSION_XML="    <key>DOCKER_API_VERSION</key>
    <string>${DOCKER_API_VERSION_VALUE}</string>"
else
  DOCKER_API_VERSION_XML=""
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
    <string>${REPO_ROOT}/scripts/retention_runner.sh</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${REPO_ROOT}</string>
  <key>StartInterval</key>
  <integer>${INTERVAL_SECONDS}</integer>
  <key>RunAtLoad</key>
  ${RUN_AT_LOAD_XML}
  <key>StandardOutPath</key>
  <string>${LOG_DIR}/retention-runner.log</string>
  <key>StandardErrorPath</key>
  <string>${LOG_DIR}/retention-runner.err</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${PATH}</string>
    <key>RETENTION_INTERVAL_SECONDS</key>
    <string>${INTERVAL_SECONDS}</string>
${DOCKER_API_VERSION_XML}
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
