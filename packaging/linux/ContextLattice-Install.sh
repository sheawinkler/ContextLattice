#!/usr/bin/env bash
set -euo pipefail
umask 077

EXPECTED_RELEASE_LANE="@RELEASE_LANE@"
if [[ -z "${INSTALL_DIR:-}" && -n "${HOME:-}" ]]; then
  INSTALL_DIR="${HOME}/ContextLattice"
else
  INSTALL_DIR="${INSTALL_DIR:-}"
fi
PAYLOAD_DIR="${PAYLOAD_DIR:-}"
FULL_MODE="${FULL_MODE:-0}"
EXTRACT_ONLY=0
NO_LAUNCH=0
ALLOW_PAID_TO_PUBLIC_DOWNGRADE=0
TMP_EXTRACT=""
TRANSACTION_DIR=""
BACKUP_MOVED=0
STAGE_PUBLISHED=0
COMMITTED=0
HAD_EXISTING_INSTALL=0

usage() {
  cat <<USAGE
Usage: ContextLattice-Install.sh [options]

Options:
  --full                 Start full compose (default is lite)
  --install-dir PATH     Install/update path (default: \$HOME/ContextLattice)
  --extract-only         Verify, replace, reconcile required posture, then exit
  --no-launch            Install and reconcile required posture without launching
  --allow-paid-to-public-downgrade
                         Explicitly replace an installed paid tree with public files
                         while preserving only .env, .data, data, and backups
  -h, --help             Show this help
USAGE
}

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

warn() {
  echo "WARN: $*" >&2
}

require_arg() {
  [[ $# -ge 2 && -n "${2:-}" ]] || fail "$1 requires a value."
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --full)
      FULL_MODE=1
      shift
      ;;
    --install-dir|--target)
      require_arg "$@"
      INSTALL_DIR="$2"
      shift 2
      ;;
    --extract-only)
      EXTRACT_ONLY=1
      NO_LAUNCH=1
      shift
      ;;
    --no-launch)
      NO_LAUNCH=1
      shift
      ;;
    --allow-paid-to-public-downgrade)
      ALLOW_PAID_TO_PUBLIC_DOWNGRADE=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[[ -n "${INSTALL_DIR}" ]] || fail "HOME is unset; pass --install-dir PATH."

[[ "${EXPECTED_RELEASE_LANE}" == "public" ]] || fail "public installer lane was not baked at build time."
EXPECTED_SOURCE_REPOSITORY="sheawinkler/ContextLattice"
EXPECTED_SOURCE_REF="refs/heads/main"

cleanup() {
  local status=$?
  set +e
  trap - EXIT HUP INT TERM

  if [[ "${COMMITTED}" != "1" ]]; then
    if [[ "${BACKUP_MOVED}" == "1" && -d "${TRANSACTION_DIR}/previous" ]]; then
      if [[ -e "${INSTALL_DIR}" || -L "${INSTALL_DIR}" ]]; then
        rm -rf -- "${INSTALL_DIR}"
      fi
      if ! mv -- "${TRANSACTION_DIR}/previous" "${INSTALL_DIR}"; then
        warn "automatic rollback failed; preserved previous install at ${TRANSACTION_DIR}/previous"
        TRANSACTION_DIR=""
      fi
    elif [[ "${HAD_EXISTING_INSTALL}" == "0" && "${STAGE_PUBLISHED}" == "1" ]]; then
      rm -rf -- "${INSTALL_DIR}"
    fi
  fi

  [[ -z "${TMP_EXTRACT}" ]] || rm -rf -- "${TMP_EXTRACT}"
  [[ -z "${TRANSACTION_DIR}" ]] || rm -rf -- "${TRANSACTION_DIR}"
  exit "${status}"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

metadata_value() {
  local file="$1"
  local key="$2"
  sed -nE "s/^[[:space:]]*\"${key}\":[[:space:]]*\"([^\"]*)\",?[[:space:]]*$/\1/p" "${file}" | head -n 1
}

sha256_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${path}" | awk '{print tolower($1)}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${path}" | awk '{print tolower($1)}'
  else
    fail "sha256sum or shasum is required to verify the embedded payload."
  fi
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -z "${PAYLOAD_DIR}" ]]; then
  if [[ -d "${SCRIPT_DIR}/payload" ]]; then
    PAYLOAD_DIR="${SCRIPT_DIR}/payload"
  elif [[ -d "${SCRIPT_DIR}/ContextLattice.app/Contents/Resources/payload" ]]; then
    PAYLOAD_DIR="${SCRIPT_DIR}/ContextLattice.app/Contents/Resources/payload"
  else
    PAYLOAD_DIR="${SCRIPT_DIR}/payload"
  fi
fi

ARCHIVE_PATH="${PAYLOAD_DIR}/contextlattice-payload.tar.gz"
CHECKSUM_PATH="${ARCHIVE_PATH}.sha256"
METADATA_PATH="${PAYLOAD_DIR}/contextlattice-release.json"

if [[ ! -f "${ARCHIVE_PATH}" || ! -f "${CHECKSUM_PATH}" || ! -f "${METADATA_PATH}" ]]; then
  fail "embedded release payload is missing or incomplete."
fi

metadata_size="$(wc -c < "${METADATA_PATH}" | tr -d '[:space:]')"
[[ "${metadata_size}" =~ ^[0-9]+$ && "${metadata_size}" -le 4096 ]] || \
  fail "release metadata exceeds its 4096-byte bound."

metadata_schema="$(metadata_value "${METADATA_PATH}" schema_id)"
metadata_lane="$(metadata_value "${METADATA_PATH}" lane)"
metadata_tag="$(metadata_value "${METADATA_PATH}" tag)"
metadata_commit="$(metadata_value "${METADATA_PATH}" commit)"
metadata_release_ref="$(metadata_value "${METADATA_PATH}" release_ref)"
metadata_source="$(metadata_value "${METADATA_PATH}" source)"
metadata_source_repository="$(metadata_value "${METADATA_PATH}" approved_source_repository)"
metadata_source_ref="$(metadata_value "${METADATA_PATH}" approved_source_ref)"

[[ "${metadata_schema}" == "contextlattice_release_payload.v2" ]] || \
  fail "unsupported release metadata schema: ${metadata_schema:-missing}"
[[ "${metadata_lane}" == "${EXPECTED_RELEASE_LANE}" ]] || \
  fail "release lane mismatch: installer=${EXPECTED_RELEASE_LANE}, payload=${metadata_lane:-missing}"
[[ "${metadata_commit}" =~ ^[0-9a-f]{40}$ ]] || fail "release metadata commit is invalid."
[[ "${metadata_tag}" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$ ]] || fail "release metadata tag is invalid."
[[ "${metadata_release_ref}" == "refs/tags/${metadata_tag}" ]] || fail "release metadata tag ref is invalid."
[[ "${metadata_source}" == "approved_lane_tagged_checkout" ]] || fail "release metadata source is invalid."
[[ "${metadata_source_repository}" == "${EXPECTED_SOURCE_REPOSITORY}" ]] || \
  fail "release source repository mismatch for ${EXPECTED_RELEASE_LANE} lane."
[[ "${metadata_source_ref}" == "${EXPECTED_SOURCE_REF}" ]] || \
  fail "release source ref mismatch for ${EXPECTED_RELEASE_LANE} lane."
if [[ "${metadata_tag}" != *-public && ! "${metadata_tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  fail "public release metadata tag must be vX.Y.Z or end with '-public'."
fi

checksum_size="$(wc -c < "${CHECKSUM_PATH}" | tr -d '[:space:]')"
[[ "${checksum_size}" =~ ^[0-9]+$ && "${checksum_size}" -le 256 ]] || \
  fail "payload checksum file exceeds its 256-byte bound."
expected_checksum="$(awk 'NR == 1 {print tolower($1)}' "${CHECKSUM_PATH}")"
checksum_name="$(awk 'NR == 1 {print $2}' "${CHECKSUM_PATH}")"
[[ "${expected_checksum}" =~ ^[0-9a-f]{64}$ && "${checksum_name}" == "contextlattice-payload.tar.gz" ]] || \
  fail "payload checksum file is invalid."
actual_checksum="$(sha256_file "${ARCHIVE_PATH}")"
[[ "${actual_checksum}" == "${expected_checksum}" ]] || fail "embedded payload checksum mismatch."

if ! tar -tzf "${ARCHIVE_PATH}" | awk '
  BEGIN { ok = 1 }
  {
    path = $0
    if (path !~ /^contextlattice\// || path ~ /^\// || path ~ /(^|\/)\.\.($|\/)/) {
      ok = 0
    }
  }
  END { exit(ok ? 0 : 1) }
'; then
  fail "embedded payload contains an unsafe archive path."
fi
if ! tar -tvzf "${ARCHIVE_PATH}" | awk '
  { type = substr($1, 1, 1); if (type != "-" && type != "d") bad = 1 }
  END { exit(bad ? 1 : 0) }
'; then
  fail "embedded payload contains a link or special file."
fi

TMP_EXTRACT="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-install.XXXXXX")"
tar -xzf "${ARCHIVE_PATH}" -C "${TMP_EXTRACT}"
PAYLOAD_ROOT="${TMP_EXTRACT}/contextlattice"
[[ -d "${PAYLOAD_ROOT}" ]] || fail "embedded payload root is missing."
[[ -f "${PAYLOAD_ROOT}/.contextlattice-release.json" ]] || fail "embedded payload metadata is missing."
cmp -s "${METADATA_PATH}" "${PAYLOAD_ROOT}/.contextlattice-release.json" || \
  fail "embedded and installer release metadata differ."
if find "${PAYLOAD_ROOT}" -type l -print -quit | grep -q .; then
  fail "embedded payload contains a symbolic link."
fi

for forbidden_state in .env .data data backups; do
  if [[ -e "${PAYLOAD_ROOT}/${forbidden_state}" || -L "${PAYLOAD_ROOT}/${forbidden_state}" ]]; then
    fail "payload contains local environment or runtime data path: ${forbidden_state}"
  fi
done

for private_path in \
  docs/private private_docs private .ops config/runtime-license \
  services/gateway-go/cognition_activation_entitled.go \
  services/gateway-go/context_mesh_orchestration_entitled.go \
  services/gateway-go/frontier_t1_governance_entitled.go \
  services/gateway-go/frontier_t2_packet_retention_entitled.go \
  services/gateway-go/frontier_t2_proof_timeline_entitled.go \
  services/gateway-go/frontier_t3_utility_entitled.go \
  services/gateway-go/frontier_t4_retrieval_entitled.go \
  config/frontier_t1_release_provenance.v1.json; do
  if [[ -e "${PAYLOAD_ROOT}/${private_path}" || -L "${PAYLOAD_ROOT}/${private_path}" ]]; then
    fail "public payload contains a paid/private path: ${private_path}"
  fi
done
public_runtime_marker='context_policy_activation\.v1|context_mesh_orchestration\.v1|frontier_t1_governance_state\.v1|frontier_delta_packet_automation\.v1|frontier_shared_proof_timeline\.v1|frontier_t4_retrieval_governance_state\.v1|contextlattice_runtime_license_public_keys\.v1|GO_V4_(ENTITLEMENT|RUNTIME_LICENSE|MACHINE_BINDING)|runtimeLicenseVerifier|runtimeLicenseSchemaID'
for runtime_file in Dockerfile.gateway-go docker-compose.yml; do
  if [[ -f "${PAYLOAD_ROOT}/${runtime_file}" ]] && grep -Eq "${public_runtime_marker}" "${PAYLOAD_ROOT}/${runtime_file}"; then
    fail "public payload contains paid/private runtime markers in ${runtime_file}."
  fi
done
if [[ -d "${PAYLOAD_ROOT}/services/gateway-go" ]] && \
   grep -ERq "${public_runtime_marker}" "${PAYLOAD_ROOT}/services/gateway-go"; then
  fail "public payload contains paid/private gateway markers."
fi

case "${INSTALL_DIR}" in
  /*) ;;
  *) INSTALL_DIR="${PWD}/${INSTALL_DIR}" ;;
esac
INSTALL_NAME="$(basename -- "${INSTALL_DIR%/}")"
INSTALL_PARENT_INPUT="$(dirname -- "${INSTALL_DIR%/}")"
[[ -n "${INSTALL_NAME}" && "${INSTALL_NAME}" != "." && "${INSTALL_NAME}" != ".." ]] || \
  fail "install directory must name a non-root path."
mkdir -p "${INSTALL_PARENT_INPUT}"
INSTALL_PARENT="$(cd "${INSTALL_PARENT_INPUT}" && pwd -P)"
INSTALL_DIR="${INSTALL_PARENT}/${INSTALL_NAME}"
[[ "${INSTALL_DIR}" != "/" ]] || fail "refusing to install over the filesystem root."
[[ ! -L "${INSTALL_DIR}" ]] || fail "install directory cannot be a symbolic link: ${INSTALL_DIR}"
if [[ -e "${INSTALL_DIR}" && ! -d "${INSTALL_DIR}" ]]; then
  fail "install path exists but is not a directory: ${INSTALL_DIR}"
fi

if [[ -d "${INSTALL_DIR}" && ! -f "${INSTALL_DIR}/.contextlattice-release.json" ]]; then
  if [[ -d "${INSTALL_DIR}/.git" ]]; then
    command -v git >/dev/null 2>&1 || \
      fail "git is required to verify the legacy checkout before migration."
    if ! git -C "${INSTALL_DIR}" diff --quiet --ignore-submodules -- || \
       ! git -C "${INSTALL_DIR}" diff --cached --quiet --ignore-submodules -- || \
       [[ -n "$(git -C "${INSTALL_DIR}" ls-files --others --exclude-standard)" ]]; then
      fail "legacy checkout has local changes or untracked files; preserve or commit them before installer migration."
    fi
    echo "Migrating clean legacy repository checkout to a release-managed install."
  else
    unmanaged_entries="$(find "${INSTALL_DIR}" -mindepth 1 -maxdepth 1 \
      ! -name .env ! -name .data ! -name data ! -name backups -print -quit)"
    [[ -z "${unmanaged_entries}" ]] || \
      fail "existing install is unmanaged; move it aside before installing so files are not silently replaced."
  fi
fi

installed_lane=""
if [[ -f "${INSTALL_DIR}/.contextlattice-release.json" ]]; then
  installed_metadata="${INSTALL_DIR}/.contextlattice-release.json"
  installed_metadata_size="$(wc -c < "${installed_metadata}" | tr -d '[:space:]')"
  [[ "${installed_metadata_size}" =~ ^[0-9]+$ && "${installed_metadata_size}" -le 4096 ]] || \
    fail "installed release metadata is invalid; refusing an ambiguous replacement."
  installed_lane="$(metadata_value "${installed_metadata}" lane)"
  case "${installed_lane}" in
    paid|public) ;;
    *) fail "installed release lane is invalid; refusing an ambiguous replacement." ;;
  esac
fi
installed_paid_marker=0
for installed_marker in \
  config/runtime-license \
  services/gateway-go/cognition_activation_entitled.go \
  services/gateway-go/context_mesh_orchestration_entitled.go \
  services/gateway-go/frontier_t1_governance_entitled.go \
  services/gateway-go/frontier_t2_packet_retention_entitled.go \
  services/gateway-go/frontier_t2_proof_timeline_entitled.go \
  services/gateway-go/frontier_t3_utility_entitled.go \
  services/gateway-go/frontier_t4_retrieval_entitled.go; do
  if [[ -e "${INSTALL_DIR}/${installed_marker}" || -L "${INSTALL_DIR}/${installed_marker}" ]]; then
    installed_paid_marker=1
    break
  fi
done
if [[ "${installed_lane}" == "public" && "${installed_paid_marker}" == "1" ]]; then
  fail "installed public metadata contradicts paid runtime files; refusing an ambiguous replacement."
fi
if [[ -z "${installed_lane}" && "${installed_paid_marker}" == "1" ]]; then
  installed_lane="paid"
fi
if [[ "${installed_lane}" == "paid" && "${EXPECTED_RELEASE_LANE}" == "public" ]]; then
  if [[ "${ALLOW_PAID_TO_PUBLIC_DOWNGRADE}" != "1" ]]; then
    fail "paid-to-public downgrade refused; rerun with --allow-paid-to-public-downgrade to remove paid files while preserving only declared user state/config."
  fi
  echo "Authorized paid-to-public downgrade: paid files will be removed; .env, .data, data, and backups will be preserved."
fi

TRANSACTION_DIR="$(mktemp -d "${INSTALL_PARENT}/.${INSTALL_NAME}.install.XXXXXX")"
STAGED_INSTALL="${TRANSACTION_DIR}/staged"
mkdir "${STAGED_INSTALL}"
tar -C "${PAYLOAD_ROOT}" -cf - . | tar -C "${STAGED_INSTALL}" -xf -

preserve_state_path() {
  local relative="$1"
  local source_path="${INSTALL_DIR}/${relative}"
  local destination_path="${STAGED_INSTALL}/${relative}"
  [[ -e "${source_path}" || -L "${source_path}" ]] || return 0
  [[ ! -L "${source_path}" ]] || fail "preserved state path cannot be a symbolic link: ${relative}"

  if [[ "${relative}" == ".env" ]]; then
    [[ -f "${source_path}" ]] || fail "preserved .env path is not a regular file."
    cat "${source_path}" > "${destination_path}"
    chmod 600 "${destination_path}"
    return 0
  fi

  [[ -d "${source_path}" ]] || fail "preserved state path is not a directory: ${relative}"
  if find "${source_path}" -type l -print -quit | grep -q .; then
    fail "preserved state directory contains a symbolic link: ${relative}"
  fi
  cp -a "${source_path}" "${destination_path}"
}

for preserved_path in .env .data data backups; do
  preserve_state_path "${preserved_path}"
done

gen_key() {
  if command -v openssl >/dev/null 2>&1; then
    printf 'cl_%s' "$(openssl rand -hex 24)"
    return
  fi
  command -v od >/dev/null 2>&1 || fail "openssl or od is required to generate the orchestrator key."
  printf 'cl_%s' "$(LC_ALL=C od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
}

set_env_value() {
  local file="$1"
  local key="$2"
  local value="$3"
  local temp_file
  temp_file="$(mktemp "${file}.tmp.XXXXXX")"
  awk -v key="${key}" -v value="${value}" '
    BEGIN { found = 0 }
    index($0, key "=") == 1 {
      if (!found) print key "=" value
      found = 1
      next
    }
    { print }
    END { if (!found) print key "=" value }
  ' "${file}" > "${temp_file}"
  chmod 600 "${temp_file}"
  mv "${temp_file}" "${file}"
}

ENV_PATH="${STAGED_INSTALL}/.env"
if [[ ! -f "${ENV_PATH}" ]]; then
  [[ -f "${STAGED_INSTALL}/.env.example" ]] || fail "payload is missing .env.example."
  cat "${STAGED_INSTALL}/.env.example" > "${ENV_PATH}"
  chmod 600 "${ENV_PATH}"
  key="$(awk -F= '/^CONTEXTLATTICE_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' "${ENV_PATH}" | tail -n 1)"
  [[ -n "${key}" ]] || key="$(gen_key)"
  set_env_value "${ENV_PATH}" CONTEXTLATTICE_ORCHESTRATOR_API_KEY "${key}"
  set_env_value "${ENV_PATH}" CONTEXTLATTICE_ORCHESTRATOR_URL "http://127.0.0.1:8075"
  set_env_value "${ENV_PATH}" HOST_BIND_ADDRESS "127.0.0.1"
  set_env_value "${ENV_PATH}" CONTEXTLATTICE_ENV "production"
  set_env_value "${ENV_PATH}" ORCH_SECURITY_STRICT "true"
else
  echo "Preserving existing ${INSTALL_DIR}/.env secrets and custom settings."
fi

chmod 600 "${ENV_PATH}"

if [[ -d "${INSTALL_DIR}" ]]; then
  HAD_EXISTING_INSTALL=1
  BACKUP_MOVED=1
  mv -- "${INSTALL_DIR}" "${TRANSACTION_DIR}/previous"
fi
STAGE_PUBLISHED=1
mv -- "${STAGED_INSTALL}" "${INSTALL_DIR}"
COMMITTED=1

echo "ContextLattice ${metadata_lane} payload ${metadata_tag} (${metadata_commit}) installed atomically at ${INSTALL_DIR}."
if [[ "${EXTRACT_ONLY}" == "1" || "${NO_LAUNCH}" == "1" ]]; then
  exit 0
fi

command -v docker >/dev/null 2>&1 || fail "Docker with Compose v2 is required to launch ContextLattice."
compose_file="docker-compose.lite.yml"
if [[ "${FULL_MODE}" == "1" ]]; then
  compose_file="docker-compose.yml"
fi

cd "${INSTALL_DIR}"
if [[ -x scripts/install_global_agent_tools.sh ]]; then
  scripts/install_global_agent_tools.sh --quiet || warn "global agent tools were not installed."
fi

echo "Launching stack with ${compose_file} ..."
docker compose -f "${compose_file}" up -d --build

if command -v curl >/dev/null 2>&1; then
  echo "Waiting for API health..."
  for _ in {1..30}; do
    if curl -fsS "http://127.0.0.1:8075/health" >/dev/null 2>&1; then
      break
    fi
    sleep 2
  done
fi

echo "Install complete. API: http://127.0.0.1:8075 Dashboard: http://127.0.0.1:3000"
if command -v open >/dev/null 2>&1; then
  open "http://127.0.0.1:3000" >/dev/null 2>&1 || true
elif command -v xdg-open >/dev/null 2>&1; then
  xdg-open "http://127.0.0.1:3000" >/dev/null 2>&1 || true
fi
