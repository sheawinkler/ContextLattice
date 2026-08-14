#!/bin/sh
set -eu

fail() {
  printf '%s\n' "fixture boundary check failed: $1" >&2
  exit 70
}

for required in TASK_ID TASK_ATTEMPT_ID TASK_LEASE_ID TASK_WORKER_ID \
  TASK_WORKER_INSTANCE_ID TASK_LEASE_GENERATION \
  CONTEXTLATTICE_CONTEXT_SNAPSHOT_ID CONTEXTLATTICE_CONTEXT_PACK_HASH \
  CONTEXTLATTICE_SESSION_ID; do
  value="$(printenv "${required}" 2>/dev/null || true)"
  [ -n "${value}" ] || fail "missing execution identity"
done

[ "${TASK_CWD:-}" = "/workspace" ] || fail "unexpected task cwd"
[ "${TASK_WORKTREE:-}" = "/workspace" ] || fail "unexpected task worktree"
[ "${HOME:-}" = "/workspace/.home" ] || fail "unexpected task home"
[ "${TASK_NETWORK_POLICY:-}" = "[]" ] || fail "network policy is not deny-all"
[ "${TASK_AUTH_SCOPE:-}" = "none" ] || fail "credential scope is not empty"
[ "${TASK_MOUNT_POLICY:-}" = "[]" ] || fail "mount policy is not empty"
[ -z "${DOCKER_HOST:-}" ] || fail "Docker host reached the task"
[ -z "${DOCKER_CONFIG:-}" ] || fail "Docker config reached the task"
[ -z "${SSH_AUTH_SOCK:-}" ] || fail "SSH agent reached the task"
if ( : > /boundary-write-probe ) 2>/dev/null; then
  fail "container root filesystem is writable"
fi

for denied in \
  "${BOUNDARY_DENIED_HOME_PATH:-/path-not-present-home}" \
  "${BOUNDARY_DENIED_KEYCHAIN_PATH:-/path-not-present-keychain}" \
  "${BOUNDARY_DENIED_SSH_PATH:-/path-not-present-ssh}" \
  "${BOUNDARY_DENIED_DOCKER_PATH:-/path-not-present-docker}" \
  "${BOUNDARY_DENIED_CONFIG_PATH:-/path-not-present-config}" \
  /Users /Library/Keychains /root/.ssh /root/.docker /root/.config \
  /var/run/docker.sock /run/host-services/ssh-auth.sock; do
  [ ! -r "${denied}" ] || fail "undeclared host path is readable"
done

if grep -Eq '^[^[:space:]]+[[:space:]]+00000000[[:space:]]' /proc/net/route; then
  fail "container has a default route"
fi
command -v wget >/dev/null 2>&1 || fail "egress probe executable is unavailable"
if wget -T 1 -t 1 -q -O /dev/null http://198.51.100.1:9/; then
  fail "network egress succeeded"
fi

umask 077
printf '%s\n' \
  "schema_id=runner_result.v1" \
  "task_id=${TASK_ID}" \
  "attempt_id=${TASK_ATTEMPT_ID}" \
  "session_id=${CONTEXTLATTICE_SESSION_ID}" \
  "snapshot_id=${CONTEXTLATTICE_CONTEXT_SNAPSHOT_ID}" \
  "context_pack_hash=${CONTEXTLATTICE_CONTEXT_PACK_HASH}" \
  "host_home=blocked" \
  "host_keychain=blocked" \
  "host_ssh=blocked" \
  "host_docker=blocked" \
  "host_config=blocked" \
  "root_filesystem=read_only" \
  "network_egress=blocked" > runner-fixture-result.txt

printf '{"schema_id":"runner_result.v1","status":"succeeded","task_id":"%s","attempt_id":"%s","runner_version":"u3-fixture/2","tests":[{"name":"isolation-fixture","status":"passed"}],"checks":[{"name":"boundary-policy","status":"passed"}],"warnings":[],"boundary":"orbstack_oci","network_egress":"blocked","host_credentials":"blocked"}\n' \
  "${TASK_ID}" "${TASK_ATTEMPT_ID}"
