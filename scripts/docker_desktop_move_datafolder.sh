#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "This script is macOS-only (Docker Desktop)." >&2
  exit 2
fi

SETTINGS_PATH="${SETTINGS_PATH:-$HOME/Library/Group Containers/group.com.docker/settings-store.json}"
TARGET_PATH="${TARGET_PATH:-}"
APPLY=0
SKIP_COPY=0

usage() {
  cat <<'USAGE'
Usage: scripts/docker_desktop_move_datafolder.sh --target <absolute-path> [--apply] [--skip-copy]

Options:
  --target <path>   New Docker Desktop DataFolder location (must be absolute)
  --apply           Perform migration (without this, script is dry-run)
  --skip-copy       Do not rsync current data into target (use only if already copied)

Notes:
  - Script updates settings-store.json key: DataFolder
  - Script quits Docker Desktop before migration and reopens it after.
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)
      [[ $# -ge 2 ]] || { echo "Missing value for --target" >&2; exit 2; }
      TARGET_PATH="$2"
      shift 2
      ;;
    --apply)
      APPLY=1
      shift
      ;;
    --skip-copy)
      SKIP_COPY=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

[[ -n "$TARGET_PATH" ]] || { usage; exit 2; }
[[ "$TARGET_PATH" = /* ]] || { echo "--target must be an absolute path" >&2; exit 2; }
[[ -f "$SETTINGS_PATH" ]] || { echo "Docker settings not found: $SETTINGS_PATH" >&2; exit 2; }

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required." >&2
  exit 2
fi

CURRENT_PATH="$(jq -r '.DataFolder // empty' "$SETTINGS_PATH")"
if [[ -z "$CURRENT_PATH" ]]; then
  echo "Could not read DataFolder from $SETTINGS_PATH" >&2
  exit 2
fi

echo "Current DataFolder: $CURRENT_PATH"
echo "Target DataFolder:  $TARGET_PATH"

if [[ "$CURRENT_PATH" == "$TARGET_PATH" ]]; then
  echo "DataFolder already set to target. Nothing to do."
  exit 0
fi

if [[ "$APPLY" != "1" ]]; then
  echo "Dry-run only. Re-run with --apply to execute."
  exit 0
fi

mkdir -p "$TARGET_PATH"

echo ">> Quitting Docker Desktop"
osascript -e 'tell application "Docker" to quit' >/dev/null 2>&1 || true
for _ in {1..90}; do
  if docker info >/dev/null 2>&1; then
    sleep 1
    continue
  fi
  break
done

if [[ "$SKIP_COPY" != "1" ]]; then
  echo ">> Copying Docker data to target (this can take a while)"
  rsync -aH --delete "$CURRENT_PATH"/ "$TARGET_PATH"/
fi

echo ">> Updating Docker settings"
BACKUP_PATH="${SETTINGS_PATH}.bak.$(date +%Y%m%d%H%M%S)"
cp "$SETTINGS_PATH" "$BACKUP_PATH"
TMP_PATH="${SETTINGS_PATH}.tmp.$$"
jq --arg path "$TARGET_PATH" '.DataFolder = $path' "$SETTINGS_PATH" > "$TMP_PATH"
mv "$TMP_PATH" "$SETTINGS_PATH"
echo "Settings backup: $BACKUP_PATH"

echo ">> Starting Docker Desktop"
open -a Docker

echo ">> Waiting for Docker daemon"
for _ in {1..180}; do
  if docker info >/dev/null 2>&1; then
    echo "Docker is back. DataFolder migration complete."
    exit 0
  fi
  sleep 2
done

echo "Docker did not become ready in time. Check Docker Desktop UI." >&2
exit 1
