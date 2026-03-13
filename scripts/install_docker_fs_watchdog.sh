#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LABEL="com.sheawinkler.contextlattice-docker-fs-watchdog"
PLIST_PATH="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_DIR="$REPO_ROOT/logs"
INTERVAL_SECONDS="${DOCKER_FS_WATCHDOG_INTERVAL_SECONDS:-300}"
WINDOW_MINUTES="${DOCKER_FS_WATCHDOG_WINDOW_MINUTES:-20}"
USER_ID="$(id -u)"
ACTION="${1:-install}"

if ! [[ "$INTERVAL_SECONDS" =~ ^[0-9]+$ ]] || [[ "$INTERVAL_SECONDS" -le 0 ]]; then
  echo "DOCKER_FS_WATCHDOG_INTERVAL_SECONDS must be a positive integer (got: $INTERVAL_SECONDS)" >&2
  exit 2
fi

if ! [[ "$WINDOW_MINUTES" =~ ^[0-9]+$ ]] || [[ "$WINDOW_MINUTES" -le 0 ]]; then
  echo "DOCKER_FS_WATCHDOG_WINDOW_MINUTES must be a positive integer (got: $WINDOW_MINUTES)" >&2
  exit 2
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
    <string>${REPO_ROOT}/scripts/docker_fs_watchdog.sh</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${REPO_ROOT}</string>
  <key>StartInterval</key>
  <integer>${INTERVAL_SECONDS}</integer>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${LOG_DIR}/docker-fs-watchdog.runner.log</string>
  <key>StandardErrorPath</key>
  <string>${LOG_DIR}/docker-fs-watchdog.runner.err</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${PATH}</string>
    <key>DOCKER_FS_WATCHDOG_WINDOW_MINUTES</key>
    <string>${WINDOW_MINUTES}</string>
  </dict>
</dict>
</plist>
PLIST

  launchctl bootout "gui/${USER_ID}" "$PLIST_PATH" >/dev/null 2>&1 || true
  launchctl bootstrap "gui/${USER_ID}" "$PLIST_PATH"
  launchctl enable "gui/${USER_ID}/${LABEL}"
  launchctl kickstart -k "gui/${USER_ID}/${LABEL}"
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
