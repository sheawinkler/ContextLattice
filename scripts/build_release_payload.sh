#!/usr/bin/env bash
set -euo pipefail
umask 077

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${PAYLOAD_OUT_DIR:-${ROOT_DIR}/dist/payload}"
RELEASE_LANE="${RELEASE_LANE:-public}"
RELEASE_TAG="${RELEASE_TAG:-}"
RELEASE_COMMIT="${RELEASE_COMMIT:-}"
PAYLOAD_FORMATS="${PAYLOAD_FORMATS:-tar.gz zip}"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/contextlattice-payload.XXXXXX")"
STAGE_DIR="${TMP_DIR}/stage"
BUILD_OUT_DIR="${TMP_DIR}/out"
METADATA_NAME="contextlattice-release.json"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

[[ "${RELEASE_LANE}" == "public" ]] || fail "the public repository builds only RELEASE_LANE=public payloads."
SOURCE_REMOTE="public"
SOURCE_TRACKING_REF="refs/remotes/public/main"
SOURCE_REF="refs/heads/main"
SOURCE_REPOSITORY="sheawinkler/ContextLattice"

if [[ -z "${RELEASE_TAG}" ]]; then
  fail "RELEASE_TAG is required."
fi
if [[ ! "${RELEASE_TAG}" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$ ]]; then
  fail "RELEASE_TAG contains unsupported characters or exceeds 128 bytes: ${RELEASE_TAG}"
fi
if [[ "${RELEASE_TAG}" != *-public && \
      ! "${RELEASE_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  fail "public payload tags must be a stable vX.Y.Z tag or end with '-public': ${RELEASE_TAG}"
fi

command -v git >/dev/null 2>&1 || fail "git is required to build a release payload."
command -v python3 >/dev/null 2>&1 || fail "python3 is required to build deterministic payload archives."

git -C "${ROOT_DIR}" show-ref --verify --quiet "refs/tags/${RELEASE_TAG}" || \
  fail "release tag ref does not exist: refs/tags/${RELEASE_TAG}"
tag_commit="$(git -C "${ROOT_DIR}" rev-parse --verify "refs/tags/${RELEASE_TAG}^{commit}" 2>/dev/null)" || \
  fail "release tag does not resolve to a commit: ${RELEASE_TAG}"
if [[ -z "${RELEASE_COMMIT}" ]]; then
  RELEASE_COMMIT="${tag_commit}"
fi
source_commit="$(git -C "${ROOT_DIR}" rev-parse --verify "${RELEASE_COMMIT}^{commit}" 2>/dev/null)" || \
  fail "RELEASE_COMMIT does not resolve to a commit: ${RELEASE_COMMIT}"
head_commit="$(git -C "${ROOT_DIR}" rev-parse --verify HEAD)"

if [[ "${tag_commit}" != "${source_commit}" ]]; then
  fail "tag/commit mismatch: ${RELEASE_TAG}=${tag_commit}, RELEASE_COMMIT=${source_commit}"
fi
if [[ "${head_commit}" != "${source_commit}" ]]; then
  fail "checkout mismatch: HEAD=${head_commit}, tagged commit=${source_commit}"
fi

source_remote_url="$(git -C "${ROOT_DIR}" remote get-url "${SOURCE_REMOTE}" 2>/dev/null)" || \
  fail "approved ${RELEASE_LANE} source remote is missing: ${SOURCE_REMOTE}"
case "${source_remote_url}" in
  "https://github.com/${SOURCE_REPOSITORY}"|\
  "https://github.com/${SOURCE_REPOSITORY}.git"|\
  "git@github.com:${SOURCE_REPOSITORY}.git"|\
  "ssh://git@github.com/${SOURCE_REPOSITORY}.git") ;;
  *) fail "approved ${RELEASE_LANE} source remote URL mismatch: ${source_remote_url}" ;;
esac
source_ref_tip="$(git -C "${ROOT_DIR}" rev-parse --verify "${SOURCE_TRACKING_REF}^{commit}" 2>/dev/null)" || \
  fail "approved ${RELEASE_LANE} source ref is missing: ${SOURCE_TRACKING_REF}"
if ! git -C "${ROOT_DIR}" merge-base --is-ancestor "${source_commit}" "${source_ref_tip}"; then
  fail "tagged commit ${source_commit} is not reachable from approved ${RELEASE_LANE} source ref ${SOURCE_TRACKING_REF} (${source_ref_tip})."
fi

while IFS= read -r source_path; do
  case "${source_path}" in
    docs/private/*|private_docs/*|private/*|.ops/*|\
    config/runtime-license/*|\
    contextlattice-dashboard/app/api/workspace/invitations/*|\
    contextlattice-dashboard/app/api/workspace/members/*|\
    contextlattice-dashboard/app/auth/invite/*|\
    contextlattice-dashboard/lib/workspaceInvitations.ts|\
    services/gateway-go/cognition_activation_entitled.go|\
    services/gateway-go/context_mesh_orchestration_entitled.go|\
    services/gateway-go/frontier_t1_governance_entitled.go|\
    services/gateway-go/frontier_t2_packet_retention_entitled.go|\
    services/gateway-go/frontier_t2_proof_timeline_entitled.go|\
    services/gateway-go/frontier_t3_utility_entitled.go|\
    services/gateway-go/frontier_t4_retrieval_entitled.go|\
    services/gateway-go/frontier_t4_retrieval_entitled_test.go|\
    services/gateway-go/frontier_t5_policy_lab_entitled.go|\
    services/gateway-go/frontier_t5_policy_lab_entitled_test.go|\
    services/gateway-go/frontier_t6_agent_fit_entitled.go|\
    services/gateway-go/frontier_t6_agent_fit_entitled_test.go|\
    services/gateway-go/frontier_t7_portable_continuation_entitled.go|\
    services/gateway-go/frontier_t7_portable_continuation_entitled_test.go|\
    services/gateway-go/frontier_t8_skill_evolution_entitled.go|\
    services/gateway-go/frontier_t8_skill_evolution_entitled_test.go|\
    services/gateway-go/frontier_t9_continuity_zero_entitled.go|\
    services/gateway-go/frontier_t9_continuity_zero_entitled_test.go|\
    config/frontier_t1_release_provenance.v1.json|\
    docs/evals/v3.21-frontier-t4-paid-activation.json|\
    docs/evals/v3.22-frontier-t5-paid-activation.json|\
    docs/evals/v3.23-frontier-t6-paid-activation.json|\
    docs/evals/v3.24-frontier-t7-paid-activation.json|\
    docs/evals/v3.25-frontier-t8-paid-activation.json|\
    docs/evals/v3.26-frontier-t9-paid-activation.json)
      fail "public source ref contains a paid/private path: ${source_path}"
      ;;
  esac
done < <(git -C "${ROOT_DIR}" ls-tree -r --name-only "${source_commit}")

public_runtime_marker='context_policy_activation\.v1|context_mesh_orchestration\.v1|frontier_t1_governance_state\.v1|frontier_delta_packet_automation\.v1|frontier_shared_proof_timeline\.v1|frontier_t4_retrieval_governance_state\.v1|frontier_t5_policy_laboratory_governance_state\.v1|frontier_t6_agent_fit_governance_state\.v1|frontier_t7_portable_continuation_governance_state\.v1|frontier_t8_skill_evolution_governance_state\.v1|frontier_t9_continuity_zero_governance_state\.v1|contextlattice_runtime_license_public_keys\.v1|CONTEXTLATTICE_FRONTIER_T2_|CONTEXTLATTICE_FRONTIER_T5_POLICY_GOVERNANCE|CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_GOVERNANCE|CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_GOVERNANCE|CONTEXTLATTICE_FRONTIER_T8_SKILL_EVOLUTION_GOVERNANCE|CONTEXTLATTICE_FRONTIER_T9_CONTINUITY_ZERO_GOVERNANCE|GO_V4_(ENTITLEMENT|RUNTIME_LICENSE|MACHINE_BINDING)|runtimeLicenseVerifier|runtimeLicenseSchemaID'
if git -C "${ROOT_DIR}" grep -n -I -E "${public_runtime_marker}" "${source_commit}" -- \
    Dockerfile.gateway-go docker-compose.yml services/gateway-go \
    >"${TMP_DIR}/public-runtime-markers.txt" 2>/dev/null; then
  cat "${TMP_DIR}/public-runtime-markers.txt" >&2
  fail "public source ref contains paid/private runtime markers."
fi

public_paid_dashboard_marker='(/api/workspace/(members|invitations)|WorkspaceInvitation|workspaceInvitations|activeWorkspaceId|Workspace people)'
if git -C "${ROOT_DIR}" grep -n -I -E "${public_paid_dashboard_marker}" "${source_commit}" -- \
    contextlattice-dashboard/app \
    contextlattice-dashboard/components \
    contextlattice-dashboard/lib \
    contextlattice-dashboard/prisma \
    >"${TMP_DIR}/public-paid-dashboard-markers.txt" 2>/dev/null; then
  cat "${TMP_DIR}/public-paid-dashboard-markers.txt" >&2
  fail "public source ref contains paid workspace-collaboration markers."
fi

untracked_non_dist="$(
  git -C "${ROOT_DIR}" ls-files --others --exclude-standard | \
    awk '$0 !~ /^dist\// { print }'
)"
if ! git -C "${ROOT_DIR}" diff --quiet --ignore-submodules -- || \
   ! git -C "${ROOT_DIR}" diff --cached --quiet --ignore-submodules -- || \
   [[ -n "${untracked_non_dist}" ]]; then
  fail "release payload checkout is dirty; tagged production payloads require a clean checkout."
fi

case " ${PAYLOAD_FORMATS} " in
  *" tar.gz "*|*" zip "*) ;;
  *) fail "PAYLOAD_FORMATS must request 'tar.gz', 'zip', or both." ;;
esac
for format in ${PAYLOAD_FORMATS}; do
  case "${format}" in
    tar.gz|zip) ;;
    *) fail "unsupported payload format: ${format}" ;;
  esac
done

mkdir -p "${OUT_DIR}" "${STAGE_DIR}" "${BUILD_OUT_DIR}"
rm -f \
  "${OUT_DIR}/contextlattice-payload.tar.gz" \
  "${OUT_DIR}/contextlattice-payload.tar.gz.sha256" \
  "${OUT_DIR}/contextlattice-payload.zip" \
  "${OUT_DIR}/contextlattice-payload.zip.sha256" \
  "${OUT_DIR}/${METADATA_NAME}"

# The clean checkout gate guarantees worktree attributes are exactly the
# attributes from the selected tag.
git -C "${ROOT_DIR}" archive \
  --format=tar \
  --worktree-attributes \
  "${source_commit}" | tar -xf - -C "${STAGE_DIR}"

source_epoch="$(git -C "${ROOT_DIR}" show -s --format=%ct "${source_commit}")"

python3 - "${STAGE_DIR}" "${BUILD_OUT_DIR}" "${RELEASE_LANE}" "${RELEASE_TAG}" "${source_commit}" "${source_epoch}" "${PAYLOAD_FORMATS}" "${SOURCE_REPOSITORY}" "${SOURCE_REF}" <<'PY'
from __future__ import annotations

import gzip
import hashlib
import io
import json
import os
import re
import stat
import sys
import tarfile
import time
import zipfile
from pathlib import Path

stage = Path(sys.argv[1]).resolve()
out = Path(sys.argv[2]).resolve()
lane = sys.argv[3]
tag = sys.argv[4]
commit = sys.argv[5]
epoch = int(sys.argv[6])
formats = sys.argv[7].split()
source_repository = sys.argv[8]
source_ref = sys.argv[9]
metadata_name = "contextlattice-release.json"
embedded_metadata_name = ".contextlattice-release.json"

paid_markers = {
    "services/gateway-go/cognition_activation_entitled.go": "context_policy_activation.v1",
    "services/gateway-go/context_mesh_orchestration_entitled.go": "context_mesh_orchestration.v1",
    "services/gateway-go/frontier_t7_portable_continuation_entitled.go": "frontier_t7_portable_continuation_governance.v1",
    "services/gateway-go/frontier_t7_portable_continuation_entitled_test.go": "frontier_t7_portable_continuation_governance.v1",
    "docs/evals/v3.24-frontier-t7-paid-activation.json": "frontier_t7_paid_activation.v1",
    "services/gateway-go/frontier_t8_skill_evolution_entitled.go": "frontier_t8_skill_evolution_governance.v1",
    "services/gateway-go/frontier_t8_skill_evolution_entitled_test.go": "frontier_t8_skill_evolution_governance.v1",
    "docs/evals/v3.25-frontier-t8-paid-activation.json": "frontier_t8_paid_activation.v1",
    "services/gateway-go/frontier_t9_continuity_zero_entitled.go": "frontier_t9_continuity_zero_governance.v1",
    "services/gateway-go/frontier_t9_continuity_zero_entitled_test.go": "frontier_t9_continuity_zero_governance.v1",
    "docs/evals/v3.26-frontier-t9-paid-activation.json": "frontier_t9_paid_activation.v1",
}
paid_runtime_files = {
    "Dockerfile.gateway-go": "COPY config/runtime-license ./config/runtime-license",
    "config/runtime-license/public_keys.json": "contextlattice_runtime_license_public_keys.v1",
    "docker-compose.yml": "GO_V4_ENTITLEMENT_MODE",
}
required_runtime = [
    ".env.example",
    "Makefile",
    "docker-compose.lite.yml",
    "services/gateway-go",
]
for required in required_runtime:
    if not (stage / required).exists():
        raise SystemExit(f"ERROR: required runtime payload path is absent: {required}")

for relative in paid_markers:
    if (stage / relative).exists():
        raise SystemExit(f"ERROR: public payload contains a paid marker path: {relative}")
runtime_license_root = stage / "config/runtime-license"
if runtime_license_root.exists():
    raise SystemExit("ERROR: public payload contains paid runtime-license material")
for relative, marker in paid_runtime_files.items():
    marker_path = stage / relative
    if marker_path.is_file() and marker in marker_path.read_text(encoding="utf-8"):
        raise SystemExit(
            f"ERROR: public payload contains paid runtime marker '{marker}' in {relative}"
        )

metadata = {
    "approved_source_ref": source_ref,
    "approved_source_repository": source_repository,
    "commit": commit,
    "forbidden_paid_marker_paths": sorted(paid_markers),
    "forbidden_paid_runtime_paths": sorted(paid_runtime_files),
    "lane": lane,
    "payload_root": "contextlattice",
    "release_ref": f"refs/tags/{tag}",
    "required_paid_markers": [],
    "required_paid_runtime_files": [],
    "schema_id": "contextlattice_release_payload.v2",
    "source": "approved_lane_tagged_checkout",
    "tag": tag,
}
metadata_bytes = (json.dumps(metadata, indent=2, sort_keys=True) + "\n").encode("utf-8")
if len(metadata_bytes) > 4096:
    raise SystemExit("ERROR: release metadata exceeds the 4096-byte bound")
(stage / embedded_metadata_name).write_bytes(metadata_bytes)
(out / metadata_name).write_bytes(metadata_bytes)
os.utime(stage / embedded_metadata_name, (epoch, epoch))

denied_roots = {
    ".backup",
    ".contextlattice.config",
    ".github",
    ".mcp-servers",
    ".ops",
    "archive",
    "artifacts",
    "bench",
    "data",
    "dev",
    "development",
    "dist",
    "docker_compose_backup",
    "logs",
    "packaging",
    "private_docs",
    "promptfoo",
    "reports",
    "tmp",
    "trae_runs",
    "trajectories",
}
denied_parts = {
    "__pycache__",
    ".data",
    ".pytest_cache",
    ".mypy_cache",
    ".ruff_cache",
    "backups",
    "cache",
    "caches",
    "credentials",
    "evidence",
    "fixtures",
    "node_modules",
    "secrets",
    "target",
    "test",
    "test-evidence",
    "test-results",
    "test_evidence",
    "testdata",
    "tests",
}
denied_docs = {"audits", "evals", "perf", "perf-candidate-notes", "private"}
denied_root_files = {
    ".env_dev",
    ".env_prod",
    ".envrc",
    ".gitattributes",
    ".gitignore",
    ".node-version",
    ".nvmrc",
    "AGENTS.md",
    "CODE_OF_CONDUCT.md",
    "CONTRIBUTING.md",
    "glama.json",
    "justfile",
    "launch.applescript",
    "pytest.ini",
    "trae_config.template.yaml",
}
secret_suffixes = {".key", ".p12", ".pem", ".pfx"}
test_suffixes = ("_test.go", "_test.py", ".spec.ts", ".spec.tsx", ".test.ts", ".test.tsx")


def denied(relative: Path) -> str | None:
    posix = relative.as_posix()
    lowered = [part.lower() for part in relative.parts]
    name = relative.name.lower()
    if relative.parts and relative.parts[0] in denied_roots:
        return "developer/local-state root"
    if len(relative.parts) >= 2 and relative.parts[0] == "docs" and relative.parts[1] in denied_docs:
        return "private or test-evidence documentation"
    if relative.as_posix() == "services/orchestrator":
        return "legacy runtime symlink"
    if len(relative.parts) >= 2 and relative.parts[:2] == ("config", "runtime-license"):
        lowered_name = relative.name.lower()
        if "private" in lowered_name or "signing" in lowered_name:
            return "private runtime-license signing material"
    if relative.as_posix() in denied_root_files:
        return "developer policy file"
    if any(part in denied_parts for part in lowered):
        return "developer/test/cache path"
    if name == ".env" or name.startswith(".env.") or name.startswith(".env_"):
        if posix != ".env.example":
            return "environment/local secret file"
    if name.endswith(".pid") or ".bak" in name or name.endswith(test_suffixes):
        return "generated/test artifact"
    if relative.suffix.lower() in secret_suffixes:
        return "secret key material"
    return None


paths = sorted(stage.rglob("*"), key=lambda path: path.relative_to(stage).as_posix())
for path in paths:
    relative = path.relative_to(stage)
    reason = denied(relative)
    if reason:
        raise SystemExit(f"ERROR: excluded path entered payload ({reason}): {relative.as_posix()}")
    if path.is_symlink():
        raise SystemExit(f"ERROR: release payload symlinks are not allowed: {relative.as_posix()}")


def normalized_mode(path: Path) -> int:
    if path.is_dir():
        return 0o755
    return 0o755 if path.stat().st_mode & stat.S_IXUSR else 0o644


def add_tar_entry(tf: tarfile.TarFile, path: Path, archive_name: str) -> None:
    info = tarfile.TarInfo(archive_name + ("/" if path.is_dir() else ""))
    info.uid = 0
    info.gid = 0
    info.uname = "root"
    info.gname = "root"
    info.mtime = epoch
    info.mode = normalized_mode(path)
    if path.is_dir():
        info.type = tarfile.DIRTYPE
        tf.addfile(info)
        return
    data = path.read_bytes()
    info.size = len(data)
    tf.addfile(info, io.BytesIO(data))


def build_tar_gz(destination: Path) -> None:
    with destination.open("wb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, compresslevel=9, mtime=epoch) as gz:
            with tarfile.open(fileobj=gz, mode="w", format=tarfile.GNU_FORMAT) as tf:
                root = tarfile.TarInfo("contextlattice/")
                root.type = tarfile.DIRTYPE
                root.uid = root.gid = 0
                root.uname = root.gname = "root"
                root.mode = 0o755
                root.mtime = epoch
                tf.addfile(root)
                for path in paths:
                    add_tar_entry(
                        tf,
                        path,
                        "contextlattice/" + path.relative_to(stage).as_posix(),
                    )


def build_zip(destination: Path) -> None:
    zip_epoch = max(epoch, 315532800)  # ZIP timestamps cannot predate 1980.
    date_time = time.gmtime(zip_epoch)[:6]
    with zipfile.ZipFile(
        destination,
        mode="w",
        compression=zipfile.ZIP_DEFLATED,
        compresslevel=9,
        strict_timestamps=True,
    ) as zf:
        for path in paths:
            relative = "contextlattice/" + path.relative_to(stage).as_posix()
            if path.is_dir():
                relative += "/"
            info = zipfile.ZipInfo(relative, date_time=date_time)
            info.create_system = 3
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = (normalized_mode(path) & 0xFFFF) << 16
            if path.is_dir():
                info.external_attr |= 0x10
                zf.writestr(info, b"")
            else:
                zf.writestr(info, path.read_bytes())


def write_checksum(path: Path) -> None:
    digest = hashlib.sha256(path.read_bytes()).hexdigest()
    (out / f"{path.name}.sha256").write_text(
        f"{digest}  {path.name}\n", encoding="ascii"
    )


if "tar.gz" in formats:
    archive = out / "contextlattice-payload.tar.gz"
    build_tar_gz(archive)
    write_checksum(archive)
if "zip" in formats:
    archive = out / "contextlattice-payload.zip"
    build_zip(archive)
    write_checksum(archive)

print(
    json.dumps(
        {
            "ok": True,
            "lane": lane,
            "tag": tag,
            "commit": commit,
            "formats": formats,
            "output_dir": str(out),
        },
        sort_keys=True,
    )
)
PY

for artifact in \
  contextlattice-payload.tar.gz \
  contextlattice-payload.tar.gz.sha256 \
  contextlattice-payload.zip \
  contextlattice-payload.zip.sha256 \
  "${METADATA_NAME}"; do
  if [[ -f "${BUILD_OUT_DIR}/${artifact}" ]]; then
    mv "${BUILD_OUT_DIR}/${artifact}" "${OUT_DIR}/${artifact}"
  fi
done
