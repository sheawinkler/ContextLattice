#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
LABEL="${CONTEXTLATTICE_RETENTION_LAUNCHD_LABEL:-com.sheawinkler.contextlattice-retention}"
LEGACY_LABEL="${CONTEXTLATTICE_RETENTION_LEGACY_LAUNCHD_LABEL:-com.sheawinkler.memmcp-retention}"
LAUNCH_AGENTS_DIR="${CONTEXTLATTICE_RETENTION_LAUNCH_AGENTS_DIR:-$HOME/Library/LaunchAgents}"
PLIST_PATH="${LAUNCH_AGENTS_DIR}/${LABEL}.plist"
LEGACY_PLIST_PATH="${LAUNCH_AGENTS_DIR}/${LEGACY_LABEL}.plist"
LOG_DIR="${CONTEXTLATTICE_RETENTION_LOG_DIR:-$REPO_ROOT/logs}"
RUNNER_PATH="${CONTEXTLATTICE_RETENTION_RUNNER_PATH:-$REPO_ROOT/scripts/retention_runner.sh}"
WORKING_DIRECTORY="${CONTEXTLATTICE_RETENTION_WORKING_DIRECTORY:-$REPO_ROOT}"
LAUNCHCTL="${CONTEXTLATTICE_RETENTION_LAUNCHCTL:-launchctl}"
INTERVAL_SECONDS="${RETENTION_INTERVAL_SECONDS:-86400}"
RUN_AT_LOAD="${RETENTION_RUN_AT_LOAD:-0}"
DOCKER_API_VERSION_VALUE="${DOCKER_API_VERSION:-}"
USER_ID="$(id -u)"
ACTION="${1:-install}"

if ! [[ "$INTERVAL_SECONDS" =~ ^[0-9]+$ ]] || [[ "$INTERVAL_SECONDS" -le 0 ]]; then
  echo "RETENTION_INTERVAL_SECONDS must be a positive integer (got: $INTERVAL_SECONDS)" >&2
  exit 2
fi

if [[ "$RUN_AT_LOAD" != "0" && "$RUN_AT_LOAD" != "1" ]]; then
  echo "RETENTION_RUN_AT_LOAD must be 0 or 1 (got: $RUN_AT_LOAD)" >&2
  exit 2
fi

if [[ ! "$LABEL" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || [[ ! "$LEGACY_LABEL" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
  echo "Retention launchd labels may contain only letters, numbers, dots, underscores, and dashes." >&2
  exit 2
fi

if [[ "$LABEL" == "$LEGACY_LABEL" ]]; then
  echo "Current and legacy retention launchd labels must be different." >&2
  exit 2
fi

case "$LAUNCH_AGENTS_DIR" in
  /*) ;;
  *)
    echo "CONTEXTLATTICE_RETENTION_LAUNCH_AGENTS_DIR must be an absolute path." >&2
    exit 2
    ;;
esac

if ! command -v "$LAUNCHCTL" >/dev/null 2>&1; then
  echo "launchctl is required for retention LaunchAgent management: ${LAUNCHCTL}" >&2
  exit 1
fi

xml_escape() {
  local value="$1"
  value="${value//&/&amp;}"
  value="${value//</&lt;}"
  value="${value//>/&gt;}"
  value="${value//\"/&quot;}"
  value="${value//\'/&apos;}"
  printf '%s' "$value"
}

is_loaded() {
  "$LAUNCHCTL" print "gui/${USER_ID}/$1" >/dev/null 2>&1
}

wait_until_unloaded() {
  local label="$1"
  local attempt=1
  while [[ "$attempt" -le 10 ]]; do
    if ! is_loaded "$label"; then
      return 0
    fi
    sleep 0.1
    attempt=$((attempt + 1))
  done
  return 1
}

unload_agent() {
  local label="$1"
  if ! is_loaded "$label"; then
    return 0
  fi
  if ! "$LAUNCHCTL" bootout "gui/${USER_ID}/${label}" >/dev/null 2>&1; then
    echo "Refusing to continue because launchctl could not unload: ${label}" >&2
    return 1
  fi
  if ! wait_until_unloaded "$label"; then
    echo "Refusing to continue while LaunchAgent remains loaded: ${label}" >&2
    return 1
  fi
}

retire_legacy_agent() {
  unload_agent "$LEGACY_LABEL"
  if [[ -f "$LEGACY_PLIST_PATH" ]]; then
    if ! grep -q "<string>${LEGACY_LABEL}</string>" "$LEGACY_PLIST_PATH"; then
      echo "Refusing to remove unexpected legacy plist contents: ${LEGACY_PLIST_PATH}" >&2
      return 1
    fi
    rm -f "$LEGACY_PLIST_PATH"
  fi
}

source_identity() {
  local source_commit source_tree runner_sha
  case "$RUNNER_PATH" in
    /*) ;;
    *)
      echo "CONTEXTLATTICE_RETENTION_RUNNER_PATH must be an absolute path." >&2
      return 1
      ;;
  esac
  case "$WORKING_DIRECTORY" in
    /*) ;;
    *)
      echo "CONTEXTLATTICE_RETENTION_WORKING_DIRECTORY must be an absolute path." >&2
      return 1
      ;;
  esac
  if [[ ! -d "$WORKING_DIRECTORY" ]]; then
    echo "Retention working directory does not exist: ${WORKING_DIRECTORY}" >&2
    return 1
  fi
  source_commit="${CONTEXTLATTICE_RETENTION_SOURCE_COMMIT:-}"
  source_tree="${CONTEXTLATTICE_RETENTION_SOURCE_TREE:-}"
  if [[ -z "$source_commit" ]]; then
    source_commit="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || true)"
  fi
  if [[ -z "$source_tree" ]]; then
    source_tree="$(git -C "$REPO_ROOT" rev-parse 'HEAD^{tree}' 2>/dev/null || true)"
  fi
  if [[ ! "$source_commit" =~ ^[0-9a-f]{40,64}$ ]] || [[ ! "$source_tree" =~ ^[0-9a-f]{40,64}$ ]]; then
    echo "Retention install requires valid source commit and tree identities." >&2
    return 1
  fi
  if [[ ! -x "$RUNNER_PATH" ]]; then
    echo "Retention runner must exist and be executable: ${RUNNER_PATH}" >&2
    return 1
  fi
  runner_sha="$(shasum -a 256 "$RUNNER_PATH" | awk '{print $1}')"
  if [[ ! "$runner_sha" =~ ^[0-9a-f]{64}$ ]]; then
    echo "Could not compute retention runner SHA-256: ${RUNNER_PATH}" >&2
    return 1
  fi
  printf '%s\n%s\n%s\n' "$source_commit" "$source_tree" "$runner_sha"
}

write_plist() {
  local destination="$1"
  local source_commit="$2"
  local source_tree="$3"
  local runner_sha="$4"
  local run_at_load_xml docker_api_version_xml
  local plist_label plist_runner plist_working plist_log plist_path plist_commit plist_tree plist_runner_sha

  if [[ "$RUN_AT_LOAD" == "1" ]]; then
    run_at_load_xml="<true/>"
  else
    run_at_load_xml="<false/>"
  fi
  if [[ -n "$DOCKER_API_VERSION_VALUE" ]]; then
    docker_api_version_xml="    <key>DOCKER_API_VERSION</key>
    <string>$(xml_escape "$DOCKER_API_VERSION_VALUE")</string>"
  else
    docker_api_version_xml=""
  fi
  plist_label="$(xml_escape "$LABEL")"
  plist_runner="$(xml_escape "$RUNNER_PATH")"
  plist_working="$(xml_escape "$WORKING_DIRECTORY")"
  plist_log="$(xml_escape "$LOG_DIR")"
  plist_path="$(xml_escape "$PATH")"
  plist_commit="$(xml_escape "$source_commit")"
  plist_tree="$(xml_escape "$source_tree")"
  plist_runner_sha="$(xml_escape "$runner_sha")"

  cat > "$destination" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${plist_label}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${plist_runner}</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${plist_working}</string>
  <key>StartInterval</key>
  <integer>${INTERVAL_SECONDS}</integer>
  <key>RunAtLoad</key>
  ${run_at_load_xml}
  <key>StandardOutPath</key>
  <string>${plist_log}/retention-runner.log</string>
  <key>StandardErrorPath</key>
  <string>${plist_log}/retention-runner.err</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>${plist_path}</string>
    <key>RETENTION_INTERVAL_SECONDS</key>
    <string>${INTERVAL_SECONDS}</string>
    <key>CONTEXTLATTICE_RETENTION_SOURCE_COMMIT</key>
    <string>${plist_commit}</string>
    <key>CONTEXTLATTICE_RETENTION_SOURCE_TREE</key>
    <string>${plist_tree}</string>
    <key>CONTEXTLATTICE_RETENTION_RUNNER_SHA256</key>
    <string>${plist_runner_sha}</string>
${docker_api_version_xml}
  </dict>
</dict>
</plist>
PLIST
}

rollback_install() {
  local was_loaded="$1"
  local had_plist="$2"
  local backup_path="$3"
  local rollback_ok=1

  if ! unload_agent "$LABEL"; then
    echo "Retention install rollback could not unload the replacement; plist retained: ${PLIST_PATH}" >&2
    return 1
  fi
  if [[ "$had_plist" == "1" ]]; then
    mv "$backup_path" "$PLIST_PATH"
  else
    rm -f "$PLIST_PATH"
  fi
  if [[ "$was_loaded" == "1" ]]; then
    if ! "$LAUNCHCTL" bootstrap "gui/${USER_ID}" "$PLIST_PATH" >/dev/null 2>&1; then
      rollback_ok=0
    elif ! "$LAUNCHCTL" enable "gui/${USER_ID}/${LABEL}" >/dev/null 2>&1; then
      rollback_ok=0
    elif ! is_loaded "$LABEL"; then
      rollback_ok=0
    fi
  fi
  if [[ "$rollback_ok" != "1" ]]; then
    echo "Retention install failed and prior LaunchAgent could not be restored: ${LABEL}" >&2
    return 1
  fi
  echo "Retention install failed; prior LaunchAgent state restored." >&2
}

install_agent() {
  local identity source_commit source_tree runner_sha
  local temp_path backup_path had_plist=0 was_loaded=0 install_error=""

  retire_legacy_agent
  identity="$(source_identity)"
  source_commit="$(printf '%s\n' "$identity" | sed -n '1p')"
  source_tree="$(printf '%s\n' "$identity" | sed -n '2p')"
  runner_sha="$(printf '%s\n' "$identity" | sed -n '3p')"
  mkdir -p "$LAUNCH_AGENTS_DIR" "$LOG_DIR"
  temp_path="${PLIST_PATH}.tmp.$$"
  backup_path="${PLIST_PATH}.backup.$$"
  trap 'rm -f "${temp_path:-}" "${backup_path:-}"' EXIT
  write_plist "$temp_path" "$source_commit" "$source_tree" "$runner_sha"
  if command -v plutil >/dev/null 2>&1 && ! plutil -lint "$temp_path" >/dev/null; then
    echo "Generated retention LaunchAgent plist is invalid: ${temp_path}" >&2
    return 1
  fi
  if [[ -f "$PLIST_PATH" ]]; then
    cp "$PLIST_PATH" "$backup_path"
    had_plist=1
  fi
  if is_loaded "$LABEL"; then
    was_loaded=1
  fi
  unload_agent "$LABEL"
  mv "$temp_path" "$PLIST_PATH"

  if ! "$LAUNCHCTL" bootstrap "gui/${USER_ID}" "$PLIST_PATH"; then
    install_error="bootstrap_failed"
  elif ! "$LAUNCHCTL" enable "gui/${USER_ID}/${LABEL}"; then
    install_error="enable_failed"
  elif [[ "$RUN_AT_LOAD" == "1" ]] && ! "$LAUNCHCTL" kickstart -k "gui/${USER_ID}/${LABEL}"; then
    install_error="kickstart_failed"
  elif ! is_loaded "$LABEL"; then
    install_error="post_install_identity_missing"
  fi

  if [[ -n "$install_error" ]]; then
    echo "Retention LaunchAgent install failed: ${install_error}" >&2
    if ! rollback_install "$was_loaded" "$had_plist" "$backup_path"; then
      trap - EXIT
      if [[ "$had_plist" == "1" && -f "$backup_path" ]]; then
        echo "Prior plist backup retained for manual recovery: ${backup_path}" >&2
      fi
    fi
    return 1
  fi

  rm -f "$backup_path"
  trap - EXIT
  echo "Installed LaunchAgent: ${PLIST_PATH}"
  echo "Installed source: commit=${source_commit} tree=${source_tree} runner_sha256=${runner_sha}"
}

uninstall_agent() {
  retire_legacy_agent
  unload_agent "$LABEL"
  rm -f "$PLIST_PATH"
  echo "Removed LaunchAgent: ${PLIST_PATH}"
}

status_agent() {
  if is_loaded "$LEGACY_LABEL"; then
    echo "Legacy LaunchAgent is still loaded: ${LEGACY_LABEL}" >&2
    "$LAUNCHCTL" print "gui/${USER_ID}/${LEGACY_LABEL}"
    return 1
  fi
  if is_loaded "$LABEL"; then
    "$LAUNCHCTL" print "gui/${USER_ID}/${LABEL}"
    return
  fi
  if "$LAUNCHCTL" list | grep -q "${LABEL}"; then
    "$LAUNCHCTL" list | grep "${LABEL}"
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
