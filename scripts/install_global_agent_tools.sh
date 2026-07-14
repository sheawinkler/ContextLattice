#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GLOBAL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
GLOBAL_SCRIPTS_DIR="${GLOBAL_HOME}/scripts"
GLOBAL_BIN_DIR="${GLOBAL_HOME}/bin"
GLOBAL_VENV_DIR="${GLOBAL_HOME}/venv-agent-tools"
UPDATE_SHELL_PROFILE=1
INSTALL_CODEX_HOOKS=0
INSTALL_AGENT_HOOKS=1
SKIP_VENV=0
INCLUDE_DEV_PYTHON_TOOLS=0
QUIET=0

usage() {
  cat <<'USAGE'
Usage: scripts/install_global_agent_tools.sh [options]

Installs Go-native ContextLattice agent helpers to ~/.contextlattice and creates:
  contextlattice
  contextlattice-agent-tools
  contextlattice_search
  contextlattice_pack
  contextlattice_synthesis_pack
  contextlattice_synthesis_pack_v2
  contextlattice_retrieval_plan
  contextlattice_claim_write
  contextlattice_claim_query
  contextlattice_continuity_reconcile
  contextlattice_objective_transition
  contextlattice_objective_graph
  contextlattice_decision_change
  contextlattice_policy_candidate
  contextlattice_policy_evaluate
  contextlattice_policy_status
  contextlattice_skill_draft
  contextlattice_skill_evaluate
  contextlattice_skill_export
  contextlattice_skill_retire
  contextlattice_skill_foundry_status
  contextlattice_memory_graph_repair
  contextlattice_memory_graph_efficacy
  contextlattice_passport_export
  contextlattice_passport_verify
  contextlattice_passport_diff
  contextlattice_passport_replay
  contextlattice_passport_import
  contextlattice_passport_status
  contextlattice_mesh_identity
  contextlattice_mesh_grant
  contextlattice_mesh_export
  contextlattice_mesh_import
  contextlattice_mesh_status
  contextlattice_write
  contextlattice_adopt
  contextlattice_doctor
  contextlattice_agent_adapter
  contextlattice_agent_discover
  contextlattice_agent_session
  contextlattice_async_inbox_drain
  contextlattice_agent_trace
  contextlattice_run_advisor
  contextlattice_agent_runtime_proof
  contextlattice_agent_adoption_proof
  contextlattice_agent_runtime_doctor
  contextlattice_strict_runtime_native_ownership
  contextlattice_context_boundary
  contextlattice_memory_topology
  contextlattice_skills_index
  contextlattice_agent_start
  contextlattice_async_inbox_hook
  contextlattice_checkpoint
  contextlattice_*_guard wrappers
  contextlattice_pre_compaction_write
  contextlattice_post_compaction_read

Optional development-only Python helpers are installed only with
--include-dev-python-tools:
  contextlattice_agent_orchestration
  contextlattice_source_backfill
  contextlattice_codex_session_store_doctor

The compact hook runtime installs its minimal Python helpers by default because
PreCompact/PostCompact/SessionStart hooks use them even when the public CLI
surface is Go-native.

Options:
  --global-home <path>    Override installation root (default: ~/.contextlattice)
  --no-shell-profile      Do not modify shell startup files
  --install-codex-hooks   Install Codex SessionStart hooks into ~/.codex/hooks.json
  --install-agent-hooks   Install detected OMP/Mercury instruction hooks (default)
  --no-agent-hooks        Do not modify detected third-party agent instruction files
  --include-dev-python-tools
                         Also install development-only Python helper wrappers
  --skip-venv             Skip Python venv/httpx setup for dev Python helpers
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
    --install-agent-hooks)
      INSTALL_AGENT_HOOKS=1
      shift
      ;;
    --no-agent-hooks)
      INSTALL_AGENT_HOOKS=0
      shift
      ;;
    --skip-venv)
      SKIP_VENV=1
      shift
      ;;
    --include-dev-python-tools)
      INCLUDE_DEV_PYTHON_TOOLS=1
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
mkdir -p "${GLOBAL_HOME}/config/model_compat"
mkdir -p "${GLOBAL_HOME}/config/agent_contracts" "${GLOBAL_HOME}/config/agents"

HOOK_ENV_FILE="${GLOBAL_HOME}/agent_hooks.env"

upsert_hook_env_defaults() {
  mkdir -p "${GLOBAL_HOME}"
  local tmp="${HOOK_ENV_FILE}.tmp"
  local existing=""
  [[ -f "$HOOK_ENV_FILE" ]] && existing="$(cat "$HOOK_ENV_FILE")"
  {
    echo "# Local-only ContextLattice hook policy. Not part of any repo."
    printf 'export CONTEXTLATTICE_REPO_ROOT=%q\n' "$ROOT_DIR"
    printf 'export CONTEXTLATTICE_ORCHESTRATOR_URL=%q\n' "http://127.0.0.1:8075"
    printf 'export MEMMCP_ORCHESTRATOR_URL=%q\n' "http://127.0.0.1:8075"
    printf 'export CONTEXTLATTICE_AGENT_ID=%q\n' "codex_gpt5"
    printf 'export MEMMCP_AGENT_ID=%q\n' "codex_gpt5"
    if [[ -n "$existing" ]]; then
      printf '%s\n' "$existing" | awk '
        /^[[:space:]]*#[[:space:]]*Local-only ContextLattice hook policy\. Not part of any repo\.[[:space:]]*$/ { next }
        /^[[:space:]]*(export[[:space:]]+)?(CONTEXTLATTICE_REPO_ROOT|CONTEXTLATTICE_ORCHESTRATOR_URL|MEMMCP_ORCHESTRATOR_URL|CONTEXTLATTICE_AGENT_ID|MEMMCP_AGENT_ID)=/ { next }
        { print }
      '
    fi
  } > "$tmp"
  mv "$tmp" "$HOOK_ENV_FILE"
  chmod 0600 "$HOOK_ENV_FILE"
}

upsert_hook_env_defaults

agent_command_detected() {
  local command_name
  for command_name in "$@"; do
    command -v "$command_name" >/dev/null 2>&1 && return 0
  done
  return 1
}

agent_instruction_hook_block() {
  local profile="$1"
  local label="$2"
  local agent_id="$3"
  local topic_path="$4"
  cat <<EOF
<!-- >>> contextlattice-agent-install:${profile} >>>
# ContextLattice ${label} Hook

ContextLattice is the local memory and context layer for this agent.

- Profile: \`${profile}\`
- Stable agent id: \`${agent_id}\`
- Topic path: \`${topic_path}\`
- Default orchestrator: \`http://127.0.0.1:8075\`

Before substantial planning or coding, retrieve scoped context:

\`\`\`bash
contextlattice_agent_start --soft --compact
contextlattice context "current task" --project contextlattice --pretty
\`\`\`

During long work, checkpoint decisions with \`contextlattice remember "checkpoint summary" --project contextlattice\`.
Before handoff or compaction, write a concise handoff through \`contextlattice_agent_adapter handoff\`.
On completion, run \`contextlattice finish "verified result" --success --project contextlattice\`.
If ContextLattice is unreachable, continue from local evidence and state \`degraded-memory mode\` explicitly.
<!-- <<< contextlattice-agent-install:${profile} <<< -->
EOF
}

upsert_agent_instruction_hook() {
  local path="$1"
  local profile="$2"
  local label="$3"
  local agent_id="$4"
  local topic_path="$5"
  local begin="<!-- >>> contextlattice-agent-install:${profile} >>>"
  local end="<!-- <<< contextlattice-agent-install:${profile} <<< -->"
  local tmp_base tmp_next mode
  tmp_base="$(mktemp)"
  tmp_next="$(mktemp)"
  if [[ -f "$path" ]]; then
    mode="$(stat -f "%Lp" "$path" 2>/dev/null || stat -c "%a" "$path" 2>/dev/null || printf '0644')"
    local skipping=0
    while IFS= read -r line || [[ -n "$line" ]]; do
      if [[ "$line" == "$begin" ]]; then
        skipping=1
        continue
      fi
      if [[ "$line" == "$end" ]]; then
        skipping=0
        continue
      fi
      [[ "$skipping" == "1" ]] && continue
      printf '%s\n' "$line" >> "$tmp_base"
    done < "$path"
  fi
  mode="${mode:-0644}"
  mkdir -p "$(dirname "$path")"
  if [[ -s "$tmp_base" ]]; then
    awk 'NF { for (i = 1; i <= blank_count; i++) print blanks[i]; blank_count = 0; print; next } { blanks[++blank_count] = $0 }' "$tmp_base" > "$tmp_next"
    printf '\n\n' >> "$tmp_next"
  fi
  agent_instruction_hook_block "$profile" "$label" "$agent_id" "$topic_path" >> "$tmp_next"
  mv "$tmp_next" "$path"
  rm -f "$tmp_base"
  chmod "$mode" "$path"
}

install_detected_agent_instruction_hooks() {
  local installed=0
  if agent_command_detected omp || [[ -d "$HOME/.omp/agent" ]]; then
    upsert_agent_instruction_hook "$HOME/.omp/agent/AGENTS.md" "omp" "OMP" "omp_agent" "runbooks/omp-integration"
    log "Installed OMP ContextLattice instruction hook in \$HOME/.omp/agent/AGENTS.md"
    installed=1
  fi
  if agent_command_detected mercury-agent mercury || [[ -d "$HOME/.mercury" ]]; then
    upsert_agent_instruction_hook "$HOME/.mercury/soul.md" "mercury-agent" "Mercury Agent" "mercury_agent" "runbooks/mercury-agent-integration"
    log "Installed Mercury ContextLattice instruction hook in \$HOME/.mercury/soul.md"
    installed=1
  fi
  if [[ "$installed" != "1" ]]; then
    log "No detected OMP or Mercury agent instruction hooks to install."
  fi
}

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

copy_runtime_python_script() {
  local rel_path="$1"
  copy_script "${ROOT_DIR}/${rel_path}" "${GLOBAL_SCRIPTS_DIR}/${rel_path#scripts/}"
}

HOOK_RUNTIME_PYTHON_FILES=(
  scripts/agent/_common.py
  scripts/agent/audit-codex-session-store
  scripts/agent/compaction-handoff-payload
  scripts/agent/contextlattice-session
  scripts/agent_contracts.py
  scripts/agent_orchestration.py
  scripts/contextlattice_client.py
)

if [[ "$INCLUDE_DEV_PYTHON_TOOLS" == "1" ]]; then
  copy_script "${ROOT_DIR}/scripts/agent_orchestration.py" "${GLOBAL_SCRIPTS_DIR}/agent_orchestration.py"
  copy_script "${ROOT_DIR}/scripts/agent_contracts.py" "${GLOBAL_SCRIPTS_DIR}/agent_contracts.py"
  copy_script "${ROOT_DIR}/scripts/contextlattice_client.py" "${GLOBAL_SCRIPTS_DIR}/contextlattice_client.py"
  copy_script "${ROOT_DIR}/scripts/contextlattice_search.py" "${GLOBAL_SCRIPTS_DIR}/contextlattice_search.py"
  copy_script "${ROOT_DIR}/scripts/contextlattice_write.py" "${GLOBAL_SCRIPTS_DIR}/contextlattice_write.py"
fi
rm -rf "${GLOBAL_SCRIPTS_DIR}/agent_hooks"
mkdir -p "${GLOBAL_SCRIPTS_DIR}/agent_hooks"
for hook_script in "${ROOT_DIR}"/scripts/agent_hooks/*.sh; do
  copy_script "$hook_script" "${GLOBAL_SCRIPTS_DIR}/agent_hooks/$(basename "$hook_script")"
done
rm -rf "${GLOBAL_SCRIPTS_DIR}/agent"
mkdir -p "${GLOBAL_SCRIPTS_DIR}/agent"
for runtime_script in "${HOOK_RUNTIME_PYTHON_FILES[@]}"; do
  copy_runtime_python_script "$runtime_script"
done
if [[ "$INCLUDE_DEV_PYTHON_TOOLS" == "1" ]]; then
  for agent_script in "${ROOT_DIR}"/scripts/agent/*; do
    [[ -f "$agent_script" ]] || continue
    copy_script "$agent_script" "${GLOBAL_SCRIPTS_DIR}/agent/$(basename "$agent_script")"
  done
fi
for compat_config in "${ROOT_DIR}"/config/model_compat/*.json; do
  [[ -f "$compat_config" ]] || continue
  cp "$compat_config" "${GLOBAL_HOME}/config/model_compat/$(basename "$compat_config")"
  chmod 0644 "${GLOBAL_HOME}/config/model_compat/$(basename "$compat_config")"
done
for contract_config in "${ROOT_DIR}"/config/agent_contracts/*.json; do
  [[ -f "$contract_config" ]] || continue
  cp "$contract_config" "${GLOBAL_HOME}/config/agent_contracts/$(basename "$contract_config")"
  chmod 0644 "${GLOBAL_HOME}/config/agent_contracts/$(basename "$contract_config")"
done
for agent_config in "${ROOT_DIR}"/config/agents/*.json; do
  [[ -f "$agent_config" ]] || continue
  cp "$agent_config" "${GLOBAL_HOME}/config/agents/$(basename "$agent_config")"
  chmod 0644 "${GLOBAL_HOME}/config/agents/$(basename "$agent_config")"
done

if [[ "$INCLUDE_DEV_PYTHON_TOOLS" == "1" && "$SKIP_VENV" != "1" ]]; then
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

build_go_agent_tools() {
  if ! command -v go >/dev/null 2>&1; then
    echo "go is required to install Go-native ContextLattice agent tools." >&2
    exit 1
  fi
  rm -f "${GLOBAL_BIN_DIR}/contextlattice-agent-tools"
  (cd "${ROOT_DIR}/services/gateway-go" && go build -o "${GLOBAL_BIN_DIR}/contextlattice-agent-tools" ./cmd/contextlattice-agent-tools)
  chmod +x "${GLOBAL_BIN_DIR}/contextlattice-agent-tools"
}

install_go_native_link() {
  local command_name="$1"
  ln -sf contextlattice-agent-tools "${GLOBAL_BIN_DIR}/${command_name}"
}

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

cat > "${GLOBAL_BIN_DIR}/contextlattice_pack" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/contextlattice-pack"
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

cat > "${GLOBAL_BIN_DIR}/contextlattice_agent_adapter" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/contextlattice-agent-adapter"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_adopt" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/contextlattice-adopt"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_doctor" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/contextlattice-adopt"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" doctor "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_agent_runtime_proof" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/agent-runtime-proof-pack"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_agent_adoption_proof" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/agent-adoption-proof-matrix"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_agent_session" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/contextlattice-session"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_agent_trace" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/agent-run-trace"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_run_advisor" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/contextlattice-run-advisor"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_agent_runtime_doctor" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/audit-agent-runtime-install"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_strict_runtime_native_ownership" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/audit-strict-runtime-native-ownership"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_context_boundary" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/audit-context-boundary"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_memory_topology" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/audit-memory-topology"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_source_backfill" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/source-backfill-memory"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_skills_index" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/contextlattice-skills-index"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_codex_session_store_doctor" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/audit-codex-session-store"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

cat > "${GLOBAL_BIN_DIR}/contextlattice_runner_quality" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
TOOL_HOME="${CONTEXTLATTICE_GLOBAL_HOME:-$HOME/.contextlattice}"
PYTHON_BIN="${TOOL_HOME}/venv-agent-tools/bin/python"
SCRIPT_PATH="${TOOL_HOME}/scripts/agent/runner-quality"
if [[ ! -x "${PYTHON_BIN}" ]]; then
  echo "Missing ${PYTHON_BIN}. Run scripts/install_global_agent_tools.sh --include-dev-python-tools first." >&2
  exit 1
fi
exec "${PYTHON_BIN}" "${SCRIPT_PATH}" "$@"
EOF

chmod +x \
  "${GLOBAL_BIN_DIR}/contextlattice_search" \
  "${GLOBAL_BIN_DIR}/contextlattice_pack" \
  "${GLOBAL_BIN_DIR}/contextlattice_write" \
  "${GLOBAL_BIN_DIR}/contextlattice_agent_orchestration" \
  "${GLOBAL_BIN_DIR}/contextlattice_adopt" \
  "${GLOBAL_BIN_DIR}/contextlattice_doctor" \
  "${GLOBAL_BIN_DIR}/contextlattice_agent_adapter" \
  "${GLOBAL_BIN_DIR}/contextlattice_agent_session" \
  "${GLOBAL_BIN_DIR}/contextlattice_agent_trace" \
  "${GLOBAL_BIN_DIR}/contextlattice_run_advisor" \
  "${GLOBAL_BIN_DIR}/contextlattice_agent_runtime_proof" \
  "${GLOBAL_BIN_DIR}/contextlattice_agent_adoption_proof" \
  "${GLOBAL_BIN_DIR}/contextlattice_agent_runtime_doctor" \
  "${GLOBAL_BIN_DIR}/contextlattice_strict_runtime_native_ownership" \
  "${GLOBAL_BIN_DIR}/contextlattice_context_boundary" \
  "${GLOBAL_BIN_DIR}/contextlattice_memory_topology" \
  "${GLOBAL_BIN_DIR}/contextlattice_source_backfill" \
  "${GLOBAL_BIN_DIR}/contextlattice_skills_index" \
  "${GLOBAL_BIN_DIR}/contextlattice_codex_session_store_doctor" \
  "${GLOBAL_BIN_DIR}/contextlattice_runner_quality"

build_go_agent_tools

GO_NATIVE_COMMANDS=(
  contextlattice
  contextlattice_search
  contextlattice_pack
  contextlattice_synthesis_pack
  contextlattice_synthesis_pack_v2
  contextlattice_retrieval_plan
  contextlattice_claim_write
  contextlattice_claim_query
  contextlattice_continuity_reconcile
  contextlattice_objective_transition
  contextlattice_objective_graph
  contextlattice_decision_change
  contextlattice_policy_candidate
  contextlattice_policy_evaluate
  contextlattice_policy_status
  contextlattice_skill_draft
  contextlattice_skill_evaluate
  contextlattice_skill_export
  contextlattice_skill_retire
  contextlattice_skill_foundry_status
  contextlattice_memory_graph_repair
  contextlattice_memory_graph_efficacy
  contextlattice_passport_export
  contextlattice_passport_verify
  contextlattice_passport_diff
  contextlattice_passport_replay
  contextlattice_passport_import
  contextlattice_passport_status
  contextlattice_mesh_identity
  contextlattice_mesh_grant
  contextlattice_mesh_export
  contextlattice_mesh_import
  contextlattice_mesh_status
  contextlattice_write
  contextlattice_adopt
  contextlattice_doctor
  contextlattice_agent_adapter
  contextlattice_agent_discover
  contextlattice_agent_session
  contextlattice_async_inbox_drain
  contextlattice_agent_trace
  contextlattice_run_advisor
  contextlattice_agent_runtime_proof
  contextlattice_agent_adoption_proof
  contextlattice_agent_runtime_doctor
  contextlattice_strict_runtime_native_ownership
  contextlattice_context_boundary
  contextlattice_memory_topology
  contextlattice_skills_index
  contextlattice_runner_quality
)

for command_name in "${GO_NATIVE_COMMANDS[@]}"; do
  install_go_native_link "$command_name"
done

if [[ "$INCLUDE_DEV_PYTHON_TOOLS" != "1" ]]; then
  rm -f \
    "${GLOBAL_BIN_DIR}/contextlattice_agent_orchestration" \
    "${GLOBAL_BIN_DIR}/contextlattice_source_backfill" \
    "${GLOBAL_BIN_DIR}/contextlattice_codex_session_store_doctor"
fi

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
    source "\$env_file" >/dev/null
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
write_hook_wrapper contextlattice_async_inbox_hook contextlattice_async_inbox_drain.sh
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
  copy_script "${ROOT_DIR}/config/codex/contextlattice_pre_compaction_write.sh" "$HOME/.codex/hooks/contextlattice_pre_compaction_write.sh"
  copy_script "${ROOT_DIR}/config/codex/contextlattice_post_compaction_read.sh" "$HOME/.codex/hooks/contextlattice_post_compaction_read.sh"
  python3 - "$HOME/.codex/hooks.json" \
    "$HOME/.codex/hooks/contextlattice_agent_start.sh" \
    "$HOME/.codex/hooks/contextlattice_pre_compaction_write.sh" \
    "$HOME/.codex/hooks/contextlattice_post_compaction_read.sh" <<'PY'
import json
import hashlib
import pathlib
import sys

hooks_path = pathlib.Path(sys.argv[1])
agent_start = sys.argv[2]
pre_compact = sys.argv[3]
post_compact = sys.argv[4]
config_path = hooks_path.with_name("config.toml")
event_labels = {
    "SessionStart": "session_start",
    "PreCompact": "pre_compact",
    "PostCompact": "post_compact",
}
try:
    payload = json.loads(hooks_path.read_text()) if hooks_path.exists() else {}
except Exception:
    payload = {}
root = payload.setdefault("hooks", {})

def hook_group(event, matcher):
    groups = root.setdefault(event, [])
    for item in groups:
        if isinstance(item, dict) and item.get("matcher") == matcher:
            return item
    item = {"matcher": matcher, "hooks": []}
    groups.append(item)
    return item

def upsert(event, matcher, command, timeout, status):
    hooks = hook_group(event, matcher).setdefault("hooks", [])
    hooks[:] = [
        hook for hook in hooks
        if not (isinstance(hook, dict) and str(hook.get("command", "")).endswith("/caveman_mode.sh"))
    ]
    for hook in hooks:
        if isinstance(hook, dict) and hook.get("command") == command:
            hook.update({"type": "command", "timeout": timeout, "statusMessage": status})
            return
    hooks.append({"type": "command", "command": command, "timeout": timeout, "statusMessage": status})

upsert("SessionStart", "startup|resume", agent_start, 200, "Running ContextLattice agent start hooks")
upsert("PreCompact", ".*", pre_compact, 200, "Writing ContextLattice compaction checkpoint")
upsert("PostCompact", ".*", post_compact, 200, "Reading ContextLattice compaction checkpoint")
hooks_path.write_text(json.dumps(payload, indent=2) + "\n")

def hook_hash(event, matcher, hook):
    normalized = {
        "type": "command",
        "command": str(hook.get("command") or ""),
        "timeout": max(1, int(hook.get("timeout") or 600)),
        "async": bool(hook.get("async", False)),
    }
    if hook.get("statusMessage") is not None:
        normalized["statusMessage"] = str(hook.get("statusMessage"))
    identity = {"event_name": event_labels[event], "hooks": [normalized]}
    if matcher is not None:
        identity["matcher"] = matcher
    raw = json.dumps(identity, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return "sha256:" + hashlib.sha256(raw).hexdigest()

state_entries = {}
for event in ("SessionStart", "PreCompact", "PostCompact"):
    for group_index, group in enumerate(root.get(event) or []):
        if not isinstance(group, dict):
            continue
        matcher = group.get("matcher")
        matcher = str(matcher) if matcher is not None else None
        for hook_index, hook in enumerate(group.get("hooks") or []):
            if not isinstance(hook, dict) or hook.get("type") != "command":
                continue
            command = str(hook.get("command") or "")
            if command in {agent_start, pre_compact, post_compact}:
                key = f"{hooks_path}:{event_labels[event]}:{group_index}:{hook_index}"
                state_entries[key] = hook_hash(event, matcher, hook)

text = config_path.read_text() if config_path.exists() else ""
managed_prefixes = tuple(f"{hooks_path}:{label}:" for label in event_labels.values())
lines = text.splitlines()
kept = []
i = 0
while i < len(lines):
    line = lines[i]
    if line.startswith('[hooks.state."') and line.endswith('"]'):
        key = line[len('[hooks.state."'):-2]
        if any(key.startswith(prefix) for prefix in managed_prefixes):
            i += 1
            while i < len(lines) and not lines[i].startswith("["):
                i += 1
            continue
    kept.append(line)
    i += 1
text = "\n".join(kept).rstrip() + "\n"
if "[hooks.state]" not in text:
    text += "\n[hooks.state]\n"
for key, value in sorted(state_entries.items()):
    text += f'\n[hooks.state."{key}"]\ntrusted_hash = "{value}"\n'
config_path.write_text(text)
PY
  log "Installed Codex SessionStart, PreCompact, and PostCompact hooks in $HOME/.codex/hooks.json"
fi

if [[ "$INSTALL_AGENT_HOOKS" == "1" ]]; then
  install_detected_agent_instruction_hooks
fi

log "Installed global ContextLattice tools:"
log "  - ${GLOBAL_BIN_DIR}/contextlattice-agent-tools"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_search"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_pack"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_synthesis_pack"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_synthesis_pack_v2"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_retrieval_plan"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_claim_write"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_claim_query"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_policy_candidate"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_policy_evaluate"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_policy_status"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_skill_draft"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_skill_evaluate"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_skill_export"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_skill_retire"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_skill_foundry_status"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_memory_graph_repair"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_memory_graph_efficacy"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_passport_export"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_passport_verify"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_passport_diff"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_passport_replay"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_passport_import"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_passport_status"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_mesh_identity"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_mesh_grant"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_mesh_export"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_mesh_import"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_mesh_status"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_write"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_adopt"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_doctor"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_adapter"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_discover"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_session"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_async_inbox_drain"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_trace"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_run_advisor"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_runtime_proof"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_adoption_proof"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_runtime_doctor"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_strict_runtime_native_ownership"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_context_boundary"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_memory_topology"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_skills_index"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_start"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_async_inbox_hook"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_checkpoint"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_pre_compaction_write"
log "  - ${GLOBAL_BIN_DIR}/contextlattice_post_compaction_read"
if [[ "$INCLUDE_DEV_PYTHON_TOOLS" == "1" ]]; then
  log "  - ${GLOBAL_BIN_DIR}/contextlattice_agent_orchestration"
  log "  - ${GLOBAL_BIN_DIR}/contextlattice_source_backfill"
  log "  - ${GLOBAL_BIN_DIR}/contextlattice_codex_session_store_doctor"
fi
log ""
log "Open a new shell (or run: export PATH=\"\$HOME/.contextlattice/bin:\$PATH\") then test:"
log "  contextlattice doctor --pretty"
log "  contextlattice context 'release readiness' --project contextlattice --pretty"
log "  contextlattice resume --project contextlattice --pretty"
log "  contextlattice remember 'checkpoint summary' --project contextlattice --pretty"
log "  contextlattice finish 'verified result' --success --project contextlattice --pretty"
log "  contextlattice_search -h"
log "  contextlattice_pack 'release readiness' --project contextlattice --pretty"
log "  contextlattice_synthesis_pack 'release readiness' --project contextlattice --pretty"
log "  contextlattice_synthesis_pack_v2 'release readiness' --project contextlattice --pretty"
log "  contextlattice_retrieval_plan 'release readiness' --project contextlattice --pretty"
log "  contextlattice_claim_query 'current release state' --project contextlattice --pretty"
log "  contextlattice_policy_candidate --project contextlattice --pretty"
log "  contextlattice_policy_status --pretty"
log "  contextlattice_skill_foundry_status --pretty"
log "  contextlattice_memory_graph_repair --project contextlattice --pretty"
log "  contextlattice_passport_export 'portable task context' --project contextlattice --output passport.json --pretty"
log "  contextlattice_passport_verify --file passport.json --pretty"
log "  contextlattice_mesh_identity --pretty"
log "  contextlattice_mesh_status --pretty"
log "  contextlattice_write -h"
log "  contextlattice_agent_adapter profiles"
log "  contextlattice_adopt status --pretty"
log "  contextlattice_doctor --agents codex --skip-provider-smoke --pretty"
log "  contextlattice_adopt proof --agents codex --skip-provider-smoke --pretty"
log "  contextlattice_agent_runtime_proof --pretty"
log "  contextlattice_agent_adoption_proof --skip-provider-smoke --progress --pretty"
log "  contextlattice_strict_runtime_native_ownership --pretty"
log "  contextlattice_context_boundary --pretty"
log "  contextlattice_agent_session runtime --pretty"
log "  contextlattice_async_inbox_drain --session-id <session-id>"
log "  contextlattice_agent_trace --session-id <session-id> --tree"
log "  contextlattice_run_advisor 'current task context' --pretty"
log "  contextlattice_memory_topology --pretty"
log "  contextlattice_skills_index search 'agent runtime' --pretty"
log "  contextlattice_runner_quality --pretty"
log "  contextlattice_async_inbox_hook --session-id <session-id>"
if [[ "$INCLUDE_DEV_PYTHON_TOOLS" == "1" ]]; then
  log "  contextlattice_source_backfill --source jsonl --path data.jsonl --project my-project --pretty"
fi
log "  contextlattice_agent_start -h"
