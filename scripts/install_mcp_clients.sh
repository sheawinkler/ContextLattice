#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE_DIR="$ROOT_DIR/configs"

backup_and_copy() {
  local src="$1" dest="$2"
  if [[ ! -f "$src" ]]; then
    echo "[warn] template $src missing; skipping"
    return
  fi
  mkdir -p "$(dirname "$dest")"
  if [[ -f "$dest" ]]; then
    local stamp
    stamp=$(date +%Y%m%d_%H%M%S)
    cp "$dest" "$dest.bak.$stamp"
  fi
  cp "$src" "$dest"
  echo "[ok] wrote $dest"
}

WINDSURF_CONF="${WINDSURF_CONF:-$HOME/.codeium/windsurf/mcp_config.json}"
CLINE_CONF="${CLINE_CONF:-$HOME/Library/Application Support/Code/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json}"
CURSOR_CONF="${CURSOR_CONF:-$HOME/Library/Application Support/Cursor/User/globalStorage/mcp-servers.json}"
CLAUDE_CONF="${CLAUDE_CONF:-$HOME/.mcp/servers.json}"

backup_and_copy "$TEMPLATE_DIR/windsurf_mcp_config.json" "$WINDSURF_CONF"
backup_and_copy "$TEMPLATE_DIR/cline_mcp_settings.json" "$CLINE_CONF"
backup_and_copy "$TEMPLATE_DIR/cursor_mcp_servers.json" "$CURSOR_CONF"
backup_and_copy "$TEMPLATE_DIR/claude_mcp_servers.json" "$CLAUDE_CONF"

echo "All MCP client configs now point at http://127.0.0.1:53130/memorymcp/mcp. Restart each IDE to pick up the change."
