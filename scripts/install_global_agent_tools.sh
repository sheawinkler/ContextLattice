#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GLOBAL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
GLOBAL_SCRIPTS_DIR="${GLOBAL_HOME}/scripts"
GLOBAL_BIN_DIR="${GLOBAL_HOME}/bin"
GLOBAL_VENV_DIR="${GLOBAL_HOME}/venv-agent-tools"
UPDATE_SHELL_PROFILE=1
INSTALL_CODEX_HOOKS=0
SKIP_VENV=0
QUIET=0

usage() {
  cat <<'USAGE'
Usage: scripts/install_global_agent_tools.sh [options]

Installs ContextLattice agent helper scripts to ~/.contextlattice and creates:
  contextlattice_search
  contextlattice_write
  contextlattice_agent_orchestration
  contextlattice_agent_start
  contextlattice_checkpoint
  contextlattice_*_guard wrappers
  contextlattice_pre_compaction_write
  contextlattice_post_compaction_read

Options:
  --global-home <path>    Override installation root (default: ~/.contextlattice)
  --no-shell-profile      Do not modify shell startup files
  --install-codex-hooks   Install Codex SessionStart hooks into ~/.codex/hooks.json
  --skip-venv             Skip Python venv/httpx setup (wrappers expect existing venv)
  --quiet                 Reduce output noise
  -h, --help              Show this help
USAGE
}

log() {
  [[ "$QUIET" == "1" ]] && return 0
  echo "$@"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --global-home)
      [[ $# -ge 2 ]] || { echo "Missing value for --global-home" >&2; exit 2; }
      GLOBAL_HOME="$2"
      GLOBAL_SCRIPTS_DIR="${GLOBAL_HOME}/scripts"
      GLOBAL_BIN_DIR="${GLOBAL_HOME}/bin"
      GLOBAL_VENV_DIR="${GLOBAL_HOME}/venv-agent-tools"
      shift 2
      ;;
    --no-shell-profile)
      UPDATE_SHELL_PROFILE=0
      shift
      ;;
    --install-codex-hooks)
      INSTALL_CODEX_HOOKS=1
      shift
      ;;
    --skip-venv)
      SKIP_VENV=1
      shift
      ;;
    --quiet)
      QUIET=1
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

mkdir -p "${GLOBAL_SCRIPTS_DIR}" "${GLOBAL_BIN_DIR}"

copy_script() {
  local src="$1"
  local dst="$2"
  if [[ ! -f "$src" ]]; then
    echo "Missing required source script: $src" >&2
    exit 1
  fi
  cp "$src" "$dst"
  chmod +x "$dst"
}

copy_script "${ROOT_DIR}/scripts/agent_orchestration.py" "${GLOBAL_SCRIPTS_DIR}/agent_orchestration.py"
copy_script "${ROOT_DIR}/scripts/contextlattice_client.py" "${GLOBAL_SCRIPTS_DIR}/contextlattice_client.py"
copy_script "${ROOT_DIR}/scripts/contextlattice_search.py" "${GLOBAL_SCRIPTS_DIR}/contextlattice_search.py"
copy_script "${ROOT_DIR}/scripts/contextlattice_write.py" "${GLOBAL_SCRIPTS_DIR}/contextlattice_write.py"
rm -rf "${GLOBAL_SCRIPTS_DIR}/agent_hooks"
mkdir -p "${GLOBAL_SCRIPTS_DIR}/agent_hooks"
for hook_script in "${ROOT_DIR}"/scripts/agent_hooks/*.sh; do
  copy_script "$hook_script" "${GLOBAL_SCRIPTS_DIR}/agent_hooks/$(basename "$hook_script")"
done

if [[ "$SKIP_VENV" != "1" ]]; then
  if ! command -v python3 >/dev/null 2>&1; then
    echo "python3 is required to install global agent tools." >&2
    exit 1
  fi
  if [[ ! -x "${GLOBAL_VENV_DIR}/bin/python" ]]; then
    python3 -m venv "${GLOBAL_VENV_DIR}"
  fi
  if ! "${GLOBAL_VENV_DIR}/bin/python" -c "import httpx" >/dev/null 2>&1; then
    "${GLOBAL_VENV_DIR}/bin/python" -m pip install --disable-pip-version-check --quiet --upgrade pip
    "${GLOBAL_VENV_DIR}/bin/python" -m pip install --disable-pip-version-check --quiet "httpx>=0.27,<1.0"
  fi
fi

cat > "${GLOBAL_BIN_DIR}/contextlattice_search" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/contextlattice_search.py"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_write" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/contextlattice_write.py"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_agent_orchestration" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent_orchestration.py"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

chmod +x \
  "${GLOBAL_BIN_DIR}/contextlattice_search" \
  "${GLOBAL_BIN_DIR}/contextlattice_write" \
  "${GLOBAL_BIN_DIR}/contextlattice_agent_orchestration"

write_hook_wrapper() {
  local command_name="$1"
  local script_name="$2"
  cat > "${GLOBAL_BIN_DIR}/${command_name}" <<EOF
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="\${CONTEXTLATTICE_GLOBAL_HOME:-\$HOME/.contextlattice}"
SCRIPT_PATH="\${TOOL_HOME}/scripts/agent_hooks/${script_name}"
for env_file in "\${TOOL_HOME}/agent_hooks.env" "\$HOME/.codex/contextlattice_hooks.env"; do
  if [[ -f "\$env_file" ]]; then
    # shellcheck source=/dev/null
    source "\$env_file"
  fi
done
if [[ ! -x "\${SCRIPT_PATH}" ]]; then
  echo "Missing \${SCRIPT_PATH}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "\${SCRIPT_PATH}" "\$@"
EOF
  chmod +x "${GLOBAL_BIN_DIR}/${command_name}"
}

write_hook_wrapper contextlattice_agent_start agent_start.sh
write_hook_wrapper contextlattice_preflight_hook contextlattice_preflight.sh
write_hook_wrapper contextlattice_checkpoint contextlattice_checkpoint.sh
write_hook_wrapper contextlattice_git_lane_guard git_lane_guard.sh
write_hook_wrapper contextlattice_branch_lane_guard branch_lane_guard.sh
write_hook_wrapper contextlattice_rust_rebuild_gate rust_rebuild_gate.sh
write_hook_wrapper contextlattice_runtime_env_guard runtime_env_guard.sh
write_hook_wrapper contextlattice_recall_quality_gate recall_quality_gate.sh
write_hook_wrapper contextlattice_resource_pressure_guard resource_pressure_guard.sh
write_hook_wrapper contextlattice_orbstack_forward_guard orbstack_forward_guard.sh
write_hook_wrapper contextlattice_native_endpoint_smoke native_endpoint_smoke.sh
write_hook_wrapper contextlattice_recall_monitor_seed recall_monitor_seed.sh
write_hook_wrapper contextlattice_public_leak_guard public_leak_guard.sh
write_hook_wrapper contextlattice_agent_policy_pack agent_policy_pack.sh
write_hook_wrapper contextlattice_command_output_budget command_output_budget.sh
write_hook_wrapper contextlattice_pre_compaction_write contextlattice_pre_compaction_write.sh
write_hook_wrapper contextlattice_post_compaction_read contextlattice_post_compaction_read.sh

ensure_path_entry() {
  local rc_file="$1"
  local export_line='export PATH="$HOME/.contextlattice/bin:$PATH"'
  local begin_marker="# >>> contextlattice tools >>>"
  local end_marker="# <<< contextlattice tools <<<"

  [[ -f "$rc_file" ]] || touch "$rc_file"
  if rg -Fq "$begin_marker" "$rc_file"; then
    return 0
  fi
  {
    echo ""
    echo "$begin_marker"
    echo "$export_line"
    echo "$end_marker"
  } >>"$rc_file"
  log "Updated ${rc_file} with ~/.contextlattice/bin PATH entry."
}

if [[ "$UPDATE_SHELL_PROFILE" == "1" ]]; then
  ensure_path_entry "$HOME/.zshrc"
  ensure_path_entry "$HOME/.bashrc"
fi

if [[ "$INSTALL_CODEX_HOOKS" == "1" ]]; then
  mkdir -p "$HOME/.codex/hooks"
  copy_script "${ROOT_DIR}/config/codex/contextlattice_agent_start.sh" "$HOME/.codex/hooks/contextlattice_agent_start.sh"
  python3 - "$HOME/.codex/hooks.json" "$HOME/.codex/hooks/contextlattice_agent_start.sh" <<'PY'
import json
import pathlib
import sys

hooks_path = pathlib.Path(sys.argv[1])
agent_start = sys.argv[2]
try:
    payload = json.loads(hooks_path.read_text()) if hooks_path.exists() else {}
except Exception:
    payload = {}
root = payload.setdefault("hooks", {})
session = root.setdefault("SessionStart", [])
entry = None
for item in session:
    if isinstance(item, dict) and item.get("matcher") == "startup|resume":
        entry = item
        break
if entry is None:
    entry = {"matcher": "startup|resume", "hooks": []}
    session.append(entry)
hooks = entry.setdefault("hooks", [])
hooks[:] = [
    hook for hook in hooks
    if not (isinstance(hook, dict) and str(hook.get("command", "")).endswith("/caveman_mode.sh"))
]

def upsert(command, timeout, status):
    for hook in hooks:
        if isinstance(hook, dict) and hook.get("command") == command:
            hook.update({"type": "command", "timeout": timeout, "statusMessage": status})
            return
    hooks.append({"type": "command", "command": command, "timeout": timeout, "statusMessage": status})

upsert(agent_start, 90, "Running ContextLattice agent start hooks")
hooks_path.write_text(json.dumps(payload, indent=2) + "\n")
PY
  log "Installed Codex SessionStart hooks in $HOME/.codex/hooks.json"
fi

log "Installed global ContextLattice tools:"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_search"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_write"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_orchestration"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_start"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_checkpoint"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_pre_compaction_write"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_post_compaction_read"
log ""
log "Open a new shell (or run: export PATH=\"\$HOME/.contextlattice/bin:\$PATH\") then test:"
log "  contextlattice_search -h"
log "  contextlattice_write -h"
log "  contextlattice_agent_start -h"
