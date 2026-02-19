#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
LABEL="com.sheawinkler.memmcp-overview-sync"
PLIST_PATH="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_DIR="$REPO_ROOT/logs"
SYNC_INTERVAL_SECONDS="${SYNC_INTERVAL_SECONDS:-604800}"

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
    <string>${REPO_ROOT}/scripts/sync_public_overview.sh</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${REPO_ROOT}</string>
  <key>StartInterval</key>
  <integer>${SYNC_INTERVAL_SECONDS}</integer>
  <key>RunAtLoad</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${LOG_DIR}/overview-sync.log</string>
  <key>StandardErrorPath</key>
  <string>${LOG_DIR}/overview-sync.err</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${PATH}</string>
  </dict>
</dict>
</plist>
PLIST

USER_ID=$(id -u)
launchctl bootout "gui/${USER_ID}" "$PLIST_PATH" >/dev/null 2>&1 || true
launchctl bootstrap "gui/${USER_ID}" "$PLIST_PATH"
launchctl enable "gui/${USER_ID}/${LABEL}"
launchctl kickstart -k "gui/${USER_ID}/${LABEL}"

echo "Installed LaunchAgent: ${PLIST_PATH}"
