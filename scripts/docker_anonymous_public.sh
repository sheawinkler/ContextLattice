#!/usr/bin/env bash
set -euo pipefail
umask 077

# Docker Hub's current public msitools manifest, resolved from the Registry v2
# API on 2026-08-10.  The release caller and this wrapper must agree exactly;
# a tag or an unapproved digest is never accepted for the anonymous lane.
readonly PUBLIC_MSITOOLS_IMAGE="marco98/msitools@sha256:0ac5297e0691e6768e1de4d7bdecef376ecdbff41c4cd7d4f3b55c5e7d42c48e"
readonly PUBLIC_MSITOOLS_CANONICAL_IMAGE="docker.io/${PUBLIC_MSITOOLS_IMAGE}"
readonly ORBSTACK_CONTEXT_NAME="orbstack"
readonly DOCKER_COMMAND="docker"
readonly DOCKER_CONFIG_TEMPLATE="${TMPDIR:-/tmp}/contextlattice-anonymous-docker.XXXXXX"

DOCKER_CONFIG_DIR=""

usage() {
  cat >&2 <<'EOF'
usage: scripts/docker_anonymous_public.sh <run|pull> <docker arguments>

The arguments must contain exactly one approved public image reference pinned
by digest.  This wrapper never accepts login, logout, push, private images,
credential flags, or a caller-selected Docker context/configuration.
EOF
}

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "${DOCKER_CONFIG_DIR}" && -d "${DOCKER_CONFIG_DIR}" ]]; then
    local config_prefix="${DOCKER_CONFIG_TEMPLATE%XXXXXX}"
    if [[ "${DOCKER_CONFIG_DIR}" != "${config_prefix}"* ]]; then
      echo "ERROR: refusing to remove an unexpected temporary Docker config path." >&2
      status=1
    else
      rm -rf -- "${DOCKER_CONFIG_DIR}"
    fi
  fi
  exit "${status}"
}
trap cleanup EXIT

reject_unsafe_argument() {
  local argument
  for argument in "$@"; do
    case "${argument}" in
      --config|--config=*|--context|--context=*|--host|--host=*|-H)
        fail "Docker config/context/host overrides are not allowed in the anonymous public lane."
        ;;
      --username|--username=*|--password|--password=*|--password-stdin|--registry-auth)
        fail "credential arguments are not allowed in the anonymous public lane."
        ;;
      --creds|--creds=*|--secret|--secret=*|--private|--auth-required)
        fail "credential or private operations are not allowed in the anonymous public lane."
        ;;
    esac
  done
}

if [[ -n "${DOCKER_AUTH_CONFIG:-}" ]]; then
  fail "DOCKER_AUTH_CONFIG is not allowed in the anonymous public lane."
fi
if [[ -n "${DOCKER_ANONYMOUS_PUBLIC_CONTEXT:-}" && "${DOCKER_ANONYMOUS_PUBLIC_CONTEXT}" != "${ORBSTACK_CONTEXT_NAME}" ]]; then
  fail "the anonymous public lane only permits the OrbStack Docker context."
fi

approved_image_ref() {
  case "$1" in
    "${PUBLIC_MSITOOLS_IMAGE}"|"${PUBLIC_MSITOOLS_CANONICAL_IMAGE}")
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

extract_image_ref() {
  local argument
  local -a image_refs=()
  for argument in "$@"; do
    if [[ "${argument}" =~ ^[A-Za-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]]; then
      image_refs+=("${argument}")
    fi
  done

  if [[ "${#image_refs[@]}" -ne 1 ]]; then
    fail "exactly one public image reference pinned by digest is required."
  fi
  if ! approved_image_ref "${image_refs[0]}"; then
    fail "image is not an approved public release image."
  fi
  printf '%s\n' "${image_refs[0]}"
}

capture_orbstack_socket() {
  local socket
  if ! socket="$(
    env -u DOCKER_HOST -u DOCKER_CONTEXT -u DOCKER_AUTH_CONFIG \
      "${DOCKER_COMMAND}" \
      --context "${ORBSTACK_CONTEXT_NAME}" \
      context inspect "${ORBSTACK_CONTEXT_NAME}" \
      --format '{{.Endpoints.docker.Host}}'
  )"; then
    fail "could not inspect the OrbStack Docker context."
  fi

  socket="$(printf '%s\n' "${socket}" | sed -e 's/[[:space:]]*$//' | sed -n '/./{p;q;}')"
  if [[ ! "${socket}" =~ ^unix://[^[:space:]]+$ ]]; then
    fail "OrbStack Docker context did not provide a Unix socket."
  fi
  printf '%s\n' "${socket}"
}

prepare_anonymous_config() {
  DOCKER_CONFIG_DIR="$(mktemp -d "${DOCKER_CONFIG_TEMPLATE}")"
  chmod 700 "${DOCKER_CONFIG_DIR}"
  printf '%s\n' '{"auths":{}}' > "${DOCKER_CONFIG_DIR}/config.json"
  chmod 600 "${DOCKER_CONFIG_DIR}/config.json"
}

main() {
  if [[ "${#}" -lt 2 ]]; then
    usage
    exit 2
  fi

  local operation="$1"
  shift
  case "${operation}" in
    run|pull)
      ;;
    login|logout|push)
      fail "${operation} is rejected by the anonymous public lane."
      ;;
    *)
      fail "unsupported Docker operation '${operation}'; only run and pull are allowed."
      ;;
  esac

  reject_unsafe_argument "$@"
  extract_image_ref "$@" >/dev/null

  # Capture the OrbStack endpoint while the caller's Docker context/config is
  # still available.  The command below never changes the selected context.
  local orbstack_socket
  orbstack_socket="$(capture_orbstack_socket)"

  prepare_anonymous_config
  env -u DOCKER_CONTEXT -u DOCKER_AUTH_CONFIG \
    DOCKER_CONFIG="${DOCKER_CONFIG_DIR}" \
    DOCKER_HOST="${orbstack_socket}" \
    DOCKER_CLI_HINTS=false \
    "${DOCKER_COMMAND}" "${operation}" "$@"
}

main "$@"
