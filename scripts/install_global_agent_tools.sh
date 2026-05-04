#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GLOBAL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
GLOBAL_SCRIPTS_DIR="${GLOBAL_HOME}/scripts"
GLOBAL_BIN_DIR="${GLOBAL_HOME}/bin"
GLOBAL_VENV_DIR="${GLOBAL_HOME}/venv-agent-tools"
UPDATE_SHELL_PROFILE=1
SKIP_VENV=0
QUIET=0

usage() {
  cat <<'USAGE'
Usage: scripts/install_global_agent_tools.sh [options]

Installs ContextLattice agent helper scripts to ~/.contextlattice and creates:
  contextlattice_search
  contextlattice_write
  contextlattice_agent_orchestration

Options:
  --global-home <path>    Override installation root (default: ~/.contextlattice)
  --no-shell-profile      Do not modify shell startup files
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

log "Installed global ContextLattice tools:"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_search"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_write"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_orchestration"
log ""
log "Open a new shell (or run: export PATH=\"\$HOME/.contextlattice/bin:\$PATH\") then test:"
log "  contextlattice_search -h"
log "  contextlattice_write -h"
