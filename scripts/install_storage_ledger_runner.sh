#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LABEL="com.sheawinkler.contextlattice-storage-ledger"
PLIST_PATH="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_DIR="$REPO_ROOT/logs"
INTERVAL_SECONDS="${STORAGE_LEDGER_INTERVAL_SECONDS:-3600}"
RUN_AT_LOAD="${STORAGE_LEDGER_RUN_AT_LOAD:-1}"
USER_ID="$(id -u)"
ACTION="${1:-install}"

if ! [[ "$INTERVAL_SECONDS" =~ ^[0-9]+$ ]] || [[ "$INTERVAL_SECONDS" -le 0 ]]; then
  echo "STORAGE_LEDGER_INTERVAL_SECONDS must be a positive integer (got: $INTERVAL_SECONDS)" >&2
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
    <string>${REPO_ROOT}/scripts/storage_ledger_runner.sh</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${REPO_ROOT}</string>
  <key>StartInterval</key>
  <integer>${INTERVAL_SECONDS}</integer>
  <key>RunAtLoad</key>
  ${RUN_AT_LOAD_XML}
  <key>StandardOutPath</key>
  <string>${LOG_DIR}/storage-ledger-runner.log</string>
  <key>StandardErrorPath</key>
  <string>${LOG_DIR}/storage-ledger-runner.err</string>
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
