#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "ERROR: macOS DMG packaging requires Darwin (hdiutil)." >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${DIST_DIR:-${ROOT_DIR}/dist}"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-dmg.XXXXXX")"
STAGE_DIR="${TMP_DIR}/ContextLattice"
APP_NAME="ContextLattice"
DMG_NAME="${DMG_NAME:-ContextLattice-macOS-universal.dmg}"
DMG_PATH="${DIST_DIR}/${DMG_NAME}"
REPO_URL="${REPO_URL:-https://github.com/sheawinkler/ContextLattice.git}"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

mkdir -p "${DIST_DIR}" "${STAGE_DIR}"

cat > "${STAGE_DIR}/ContextLattice.command" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/sheawinkler/ContextLattice.git}"
TARGET_DIR="${TARGET_DIR:-$HOME/ContextLattice}"

echo "== ContextLattice installer =="
echo "Repo: ${REPO_URL}"
echo "Target: ${TARGET_DIR}"

if ! command -v git >/dev/null 2>&1; then
  echo "ERROR: git is required. Install Xcode Command Line Tools first." >&2
  exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: Docker is required. Install Docker Desktop, then rerun this installer." >&2
  exit 1
fi

if [[ ! -d "${TARGET_DIR}/.git" ]]; then
  git clone "${REPO_URL}" "${TARGET_DIR}"
else
  git -C "${TARGET_DIR}" pull --ff-only || true
fi

if ! command -v gmake >/dev/null 2>&1; then
  if command -v brew >/dev/null 2>&1; then
    brew install make jq ripgrep
  else
    echo "ERROR: gmake is required. Install Homebrew then run: brew install make jq ripgrep" >&2
    exit 1
  fi
fi

cd "${TARGET_DIR}"
QUICKSTART_PROFILE_DEFAULT="${QUICKSTART_PROFILE_DEFAULT:-lite}" gmake quickstart
if [[ -x ./scripts/install_global_agent_tools.sh ]]; then
  ./scripts/install_global_agent_tools.sh --quiet || true
fi

INSTR_DIR="${TARGET_DIR}/setup"
INSTR_FILE="${INSTR_DIR}/agent_smoke_write_read.md"
AGENT_INSTR_FILE="${INSTR_DIR}/agent_contextlattice_instructions.md"
UNIVERSAL_TEMPLATE_FILE="${TARGET_DIR}/docs/public_overview/templates/agents/universal.md"
mkdir -p "${INSTR_DIR}"

if [[ -f "${UNIVERSAL_TEMPLATE_FILE}" ]]; then
  cp "${UNIVERSAL_TEMPLATE_FILE}" "${AGENT_INSTR_FILE}"
else
  cat > "${AGENT_INSTR_FILE}" <<'EOF_AGENT'
# ContextLattice Agent Contract

Use ContextLattice at http://127.0.0.1:8075 for every task:
- read context before inference (`POST /memory/search`, `include_grounding=true`)
- use `POST /memory/context-pack` for broad tasks
- checkpoint key progress via `POST /memory/write`
- final recency read before final output
- use async continuation token/events when returned
- use `POST /v1/memory/neighbors` for graph-neighbor recall
EOF_AGENT
fi

cat > "${INSTR_FILE}" <<'EOF_SMOKE'
# ContextLattice Agent Smoke Test (Write -> Read)

Before running this smoke test, paste the operating contract from:
- `~/ContextLattice/setup/agent_contextlattice_instructions.md`

Use this exact sequence after install to confirm your agent can write and read memory through the orchestrator.

```bash
export CONTEXTLATTICE_ORCHESTRATOR_URL="http://127.0.0.1:8075"
export MEMMCP_ORCHESTRATOR_URL="http://127.0.0.1:8075"
export ORCH_KEY="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' "$HOME/ContextLattice/.env" | tail -1)"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
FILE_NAME="setup/smoke_${STAMP}.md"

# 1) Write memory
curl -sS -X POST "$CONTEXTLATTICE_ORCHESTRATOR_URL/memory/write" \
  -H "Content-Type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d "{\"projectName\":\"contextlattice\",\"fileName\":\"${FILE_NAME}\",\"content\":\"smoke write ${STAMP}\",\"topicPath\":\"runbooks/setup/smoke\"}" | jq .

# 2) Read memory
curl -sS -X POST "$CONTEXTLATTICE_ORCHESTRATOR_URL/memory/search" \
  -H "Content-Type: application/json" \
  -H "x-api-key: ${ORCH_KEY}" \
  -d "{\"project\":\"contextlattice\",\"query\":\"smoke write ${STAMP}\",\"topic_path\":\"runbooks/setup/smoke\",\"include_grounding\":true}" | jq .
```

Expected:
- write returns `ok: true`
- read returns at least one matching result
EOF_SMOKE

if command -v pbcopy >/dev/null 2>&1; then
  pbcopy < "${AGENT_INSTR_FILE}" || true
  echo "Copied agent operating instructions to clipboard."
fi
echo "Agent smoke instructions: ${INSTR_FILE}"
echo "Agent operating instructions: ${AGENT_INSTR_FILE}"

if [[ -x ./scripts/open_monitoring.sh ]]; then
  ./scripts/open_monitoring.sh || true
fi

echo "ContextLattice installed. Use the Monitoring.command launcher for health checks."
EOF

cat > "${STAGE_DIR}/Monitoring.command" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

TARGET_DIR="${TARGET_DIR:-$HOME/ContextLattice}"
if [[ ! -d "${TARGET_DIR}" ]]; then
  echo "ERROR: ${TARGET_DIR} not found. Run ContextLattice.command first." >&2
  exit 1
fi

cd "${TARGET_DIR}"
if [[ -x ./scripts/open_monitoring.sh ]]; then
  ./scripts/open_monitoring.sh
else
  echo "Monitoring script missing. Re-pull repo and retry." >&2
  exit 1
fi
EOF

cat > "${STAGE_DIR}/README.txt" <<EOF
ContextLattice macOS DMG Bootstrap
=================================

This DMG installs the local ContextLattice stack through the same public repo
used by technical users, but in a lower-friction launcher format.

Included:
- ContextLattice.command  : clone/update + gmake quickstart
- Monitoring.command      : open dashboard + health/status checks
- setup/agent_contextlattice_instructions.md : copy-ready agent instruction contract
- setup/agent_smoke_write_read.md : operator write/read verification flow

Requirements:
- Docker Desktop installed and running
- git (Xcode command line tools)
- Homebrew (recommended; installer will use it if gmake is missing)

Repo:
${REPO_URL}
EOF

chmod +x "${STAGE_DIR}/ContextLattice.command" "${STAGE_DIR}/Monitoring.command"

if [[ -f "${DMG_PATH}" ]]; then
  rm -f "${DMG_PATH}"
fi

hdiutil create \
  -volname "${APP_NAME}" \
  -srcfolder "${STAGE_DIR}" \
  -ov \
  -format UDZO \
  "${DMG_PATH}" >/dev/null

echo "Built DMG: ${DMG_PATH}"
