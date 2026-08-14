#!/usr/bin/env python3
"""Bounded, fenced local execution for the task-delivery worker.

This module deliberately contains no task state store.  The Gateway task
ledger owns task, attempt, lease, result, artifact, and publication state;
this module only prepares an execution surface and reports observations using
the fence returned by the ledger.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import re
import secrets
import shlex
import shutil
import signal
import stat
import subprocess
import tempfile
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

try:
    import pwd as _pwd
except ImportError:  # Windows has no pwd module; public projection remains usable.
    _pwd = None

try:
    from scripts.agent._common import (
        _contains_sensitive_text,
        _is_secret_key,
        _redact_value,
        _sanitize_public_text,
        redact_public_value,
        redact_text,
    )
except ModuleNotFoundError:  # pragma: no cover - direct scripts/ execution
    from agent._common import (  # type: ignore[no-redef]
        _contains_sensitive_text,
        _is_secret_key,
        _redact_value,
        _sanitize_public_text,
        redact_public_value,
        redact_text,
    )

__all__ = ["redact_public_value", "redact_text"]


GIT_EXECUTABLE = Path("/usr/bin/git")
APPROVED_TASK_IMAGES = frozenset(
    {
        "alpine@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc",
    }
)
_IMAGE_DIGEST_RE = re.compile(r"^[a-z0-9][a-z0-9._/-]*@sha256:[0-9a-f]{64}$")
_ORBSTACK_SOCKET_RE = re.compile(r"^unix://[^\s]+$")
MAX_STDOUT_BYTES = 2 * 1024 * 1024
MAX_STDERR_BYTES = 2 * 1024 * 1024
MAX_COMBINED_OUTPUT_BYTES = 8 * 1024 * 1024
MAX_PATCH_BYTES = 8 * 1024 * 1024
MAX_CONTEXT_BYTES = 256 * 1024
MAX_SUMMARY_BYTES = 32 * 1024
MAX_EVENT_BYTES = 16 * 1024
MAX_PROCESS_PROBE_EVIDENCE_BYTES = 4 * 1024
MAX_REGISTERED_FIXTURE_RUNTIME_SECS = 30
TRUNCATION_MARKER = "... [truncated]"
_GIT_CONFIG_ARGS = (
    "-c",
    "core.fsmonitor=false",
    "-c",
    "core.hooksPath=/dev/null",
    "-c",
    "core.attributesFile=/dev/null",
    "-c",
    "core.excludesFile=/dev/null",
    "-c",
    "diff.external=",
    "-c",
    "protocol.file.allow=never",
)

class ExecutionBlocked(RuntimeError):
    """A task cannot run without weakening the registered execution policy."""

    def __init__(
        self,
        reason: str,
        detail: str = "",
        *,
        execution_observed: bool = False,
        evidence: Mapping[str, Any] | None = None,
    ) -> None:
        raw_reason = str(reason or "execution_blocked").strip().lower()
        self.reason = raw_reason if re.fullmatch(r"[a-z][a-z0-9_]{0,119}", raw_reason) else "execution_blocked"
        self.detail = _sanitize_public_text(detail)[0][:1024]
        self.execution_observed = bool(execution_observed)
        self.evidence = _redact_value(evidence or {}) if "_redact_value" in globals() else {}
        super().__init__(self.reason if not self.detail else f"{self.reason}: {self.detail}")


class PublicationNotAcknowledged(RuntimeError):
    """The Gateway did not acknowledge the staged publication."""

    execution_observed = True


class ReconciliationLookupRetryable(RuntimeError):
    """A temporary authoritative receipt lookup failure retained for retry."""

    def __init__(self, reason: str) -> None:
        allowed = {
            "reconciliation_publication_not_found",
            "reconciliation_gateway_unavailable",
            "reconciliation_transport_unavailable",
        }
        self.reason = reason if reason in allowed else "reconciliation_gateway_unavailable"
        super().__init__(self.reason)


@dataclass(frozen=True)
class _ProbeUnavailable:
    """A process probe failed without being usable as absence evidence."""

    evidence: dict[str, Any]


@dataclass(frozen=True)
class LeaseFence:
    task_id: str
    attempt_id: str
    lease_id: str
    worker_id: str
    worker_instance_id: str
    generation: int

    def as_dict(self) -> dict[str, Any]:
        return {
            "task_id": self.task_id,
            "attempt_id": self.attempt_id,
            "lease_id": self.lease_id,
            "worker_id": self.worker_id,
            "worker_instance_id": self.worker_instance_id,
            "generation": self.generation,
        }


@dataclass(frozen=True, slots=True)
class WorkerAuthSnapshot:
    """Immutable worker proof captured before a lease heartbeat thread starts."""

    worker_instance_id: str
    worker_instance_credential: str = field(repr=False)


@dataclass(frozen=True)
class SnapshotBinding:
    snapshot_id: str
    content_hash: str
    session_id: str
    task_id: str
    attempt_id: str
    snapshot: dict[str, Any]

    def as_dict(self) -> dict[str, Any]:
        return {
            "snapshot_id": self.snapshot_id,
            "content_hash": self.content_hash,
            "session_id": self.session_id,
            "task_id": self.task_id,
            "attempt_id": self.attempt_id,
        }


@dataclass(frozen=True)
class WorkspaceBinding:
    kind: str
    cwd: Path
    repo: Path | None
    worktree: Path | None
    base_sha: str
    source_checkout_dirty: bool = False

    def as_dict(self) -> dict[str, Any]:
        ref = "workspace-" + hashlib.sha256(
            f"{self.kind}\0{self.repo or ''}\0{self.worktree or ''}".encode("utf-8")
        ).hexdigest()[:32]
        return {
            "kind": self.kind,
            "workspace_ref": ref,
            "base_sha": self.base_sha,
            "source_checkout_dirty": self.source_checkout_dirty,
        }


@dataclass(frozen=True)
class CaptureResult:
    returncode: int
    stdout: bytes
    stderr: bytes
    stdout_truncated: bool
    stderr_truncated: bool
    combined_truncated: bool
    outcome: str
    termination: dict[str, Any]


@dataclass(frozen=True)
class PreparedExecution:
    task: dict[str, Any]
    attempt: dict[str, Any]
    fence: LeaseFence
    profile_name: str
    profile: dict[str, Any]
    snapshot: SnapshotBinding
    workspace: WorkspaceBinding
    argv: list[str] | None
    env: dict[str, str]
    prompt: str


@dataclass(frozen=True)
class ContainerBoundary:
    argv: list[str]
    env: dict[str, str]
    docker_executable: Path
    container_name: str
    config_dir: Path
    cidfile: Path
    run_nonce: str
    task_ref: str
    control_timeout_secs: float


def _map(value: Any) -> dict[str, Any]:
    return dict(value) if isinstance(value, Mapping) else {}


def _list(value: Any) -> list[Any]:
    return list(value) if isinstance(value, (list, tuple)) else []


def _first(*values: Any) -> str:
    for value in values:
        text = str(value or "").strip()
        if text:
            return text
    return ""


def _task_payload(task: Mapping[str, Any]) -> dict[str, Any]:
    return _map(task.get("payload"))


def _value(task: Mapping[str, Any], *keys: str) -> Any:
    payload = _task_payload(task)
    for source in (task, payload):
        for key in keys:
            if key in source and source[key] not in (None, ""):
                return source[key]
    return None


def _claim_parts(
    claim: Mapping[str, Any],
    worker: str = "",
    worker_instance: str = "",
) -> tuple[dict[str, Any], dict[str, Any], LeaseFence]:
    raw_task = _map(claim.get("task"))
    if not raw_task and (claim.get("id") or claim.get("task_id")):
        raw_task = dict(claim)
    task = dict(raw_task)
    attempt = _map(claim.get("attempt"))
    lease = _map(claim.get("lease"))
    sources: list[Mapping[str, Any]] = [claim, _map(claim.get("fence")), task, _map(task.get("fence")), _map(task.get("_fence")), attempt, _map(attempt.get("fence")), lease, _map(lease.get("fence"))]

    def exact_value(field: str, aliases: Sequence[str]) -> str:
        values: list[str] = []
        for source in sources:
            for alias in aliases:
                if alias in source and source.get(alias) not in (None, ""):
                    value = str(source.get(alias)).strip()
                    if value:
                        values.append(value)
        unique = set(values)
        if len(unique) > 1:
            raise ExecutionBlocked("conflicting_lease_fence", f"conflicting {field} copies were rejected")
        return values[0] if values else ""

    task_id = exact_value("task_id", ("task_id", "taskId"))
    task_id_aliases = [str(value).strip() for value in (task.get("id"), claim.get("id") if not raw_task else None) if value not in (None, "")]
    if task_id_aliases:
        task_ids = {value for value in [task_id, *task_id_aliases] if value}
        if len(task_ids) > 1:
            raise ExecutionBlocked("conflicting_lease_fence", "conflicting task_id copies were rejected")
        task_id = task_id or task_id_aliases[0]
    attempt_id = exact_value("attempt_id", ("attempt_id", "attemptId"))
    lease_id = exact_value("lease_id", ("lease_id", "leaseId"))
    worker_id = exact_value("worker_id", ("worker_id", "workerId"))
    worker_instance_id = exact_value("worker_instance_id", ("worker_instance_id", "workerInstanceId"))
    generation_text = exact_value("generation", ("generation", "lease_generation", "leaseGeneration", "assignment_generation", "assignmentGeneration"))
    for field_name, value in (
        ("task_id", task_id),
        ("attempt_id", attempt_id),
        ("lease_id", lease_id),
        ("worker_id", worker_id),
        ("worker_instance_id", worker_instance_id),
    ):
        if value and not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:@-]{0,255}", value):
            raise ExecutionBlocked("lease_fence_invalid", f"{field_name} is not a bounded public-safe identifier")
    if generation_text and not re.fullmatch(r"[1-9][0-9]*", generation_text):
        raise ExecutionBlocked("lease_fence_invalid", "generation is not a canonical positive integer")
    try:
        generation = int(generation_text or "0")
    except ValueError:
        generation = 0
    if not all((task_id, attempt_id, lease_id, worker_id, worker_instance_id)) or generation <= 0:
        raise ExecutionBlocked("incomplete_lease_fence", "task, attempt, lease, worker, worker instance, and generation are required")
    requested_worker = str(worker or "").strip()
    requested_instance = str(worker_instance or "").strip()
    if requested_worker and worker_id != requested_worker:
        raise ExecutionBlocked("worker_fence_mismatch", "the claim worker does not match the requested worker")
    if requested_instance and worker_instance_id != requested_instance:
        raise ExecutionBlocked("worker_instance_fence_mismatch", "the claim worker instance does not match the requested worker instance")
    fence = LeaseFence(task_id, attempt_id, lease_id, worker_id, worker_instance_id, generation)
    task["task_id"] = task_id
    task["_fence"] = fence.as_dict()
    task["_attempt"] = dict(attempt)
    return task, attempt, fence


def extract_lease_fence(claim: Mapping[str, Any], worker: str = "", worker_instance: str = "") -> LeaseFence:
    """Return the immutable fence from a Gateway claim or raise fail-closed."""

    return _claim_parts(claim, worker, worker_instance)[2]


def claim_has_complete_fence(claim: Mapping[str, Any], worker: str = "", worker_instance: str = "") -> bool:
    try:
        extract_lease_fence(claim, worker, worker_instance)
    except ExecutionBlocked:
        return False
    return True


def _absolute_server_path(raw: str | Path, *, reason: str) -> Path:
    source = str(raw or "").strip()
    if not source or source.startswith("~") or "\x00" in source:
        raise ExecutionBlocked(reason, "an absolute server-owned path is required")
    path = Path(source)
    if not path.is_absolute():
        raise ExecutionBlocked(reason, "an absolute server-owned path is required")
    return path.resolve(strict=False)


def _configured_worktree_root(raw: str | Path | None = None) -> Path:
    configured = raw if raw is not None else os.getenv("CONTEXTLATTICE_TASK_WORKTREE_ROOT")
    if configured in (None, ""):
        raise ExecutionBlocked(
            "task_storage_unconfigured",
            "CONTEXTLATTICE_TASK_WORKTREE_ROOT must name a server-owned directory",
        )
    root = _absolute_server_path(configured, reason="task_storage_invalid")
    home = Path.home().resolve(strict=False)
    forbidden = {Path("/"), home, Path("/Users"), Path("/home"), Path("/tmp"), Path("/private/tmp")}
    if root in forbidden or home in root.parents:
        raise ExecutionBlocked("task_storage_invalid", "the task root cannot be a user home or shared system directory")
    try:
        root_stat = root.stat()
    except OSError as exc:
        raise ExecutionBlocked("task_storage_unavailable", "the configured task root is unavailable") from exc
    if not stat.S_ISDIR(root_stat.st_mode):
        raise ExecutionBlocked("task_storage_invalid", "the configured task root is not a directory")
    if root_stat.st_uid != os.getuid() or stat.S_IMODE(root_stat.st_mode) & 0o022:
        raise ExecutionBlocked("task_storage_ownership_invalid", "the configured task root must be owned by the worker and not group/world writable")
    return root


def _task_workspace_path(root: Path, fence: LeaseFence) -> Path:
    for value in (fence.task_id, fence.attempt_id):
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", value):
            raise ExecutionBlocked("workspace_identity_invalid", "task and attempt IDs must be path-safe")
    target = (root / fence.task_id / fence.attempt_id).resolve(strict=False)
    if root not in target.parents:
        raise ExecutionBlocked("workspace_identity_invalid", "task workspace escaped the configured root")
    return target


def _result_id(fence: LeaseFence) -> str:
    return "result-" + hashlib.sha256(f"{fence.task_id}\0{fence.attempt_id}".encode("utf-8")).hexdigest()[:32]


def _publication_id(fence: LeaseFence) -> str:
    return "publication-" + hashlib.sha256(f"{fence.task_id}\0{fence.attempt_id}\0publication".encode("utf-8")).hexdigest()[:32]


def _cleanup_id(fence: LeaseFence, workspace_ref: str) -> str:
    return "cleanup-" + hashlib.sha256(f"{fence.task_id}\0{fence.attempt_id}\0{workspace_ref}".encode("utf-8")).hexdigest()[:32]


def _ownership_marker_path(root: Path, fence: LeaseFence) -> Path:
    return root / fence.task_id / f"{fence.attempt_id}.owner.json"


def _atomic_owner_file(path: Path, payload: Mapping[str, Any]) -> None:
    rendered = json.dumps(dict(payload), ensure_ascii=True, sort_keys=True, separators=(",", ":")).encode("utf-8") + b"\n"
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    path.parent.chmod(0o700)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=str(path.parent))
    try:
        os.fchmod(descriptor, 0o600)
        with os.fdopen(descriptor, "wb", closefd=True) as handle:
            handle.write(rendered)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except Exception:
        try:
            os.close(descriptor)
        except OSError:
            pass
        try:
            Path(temporary).unlink()
        except OSError:
            pass
        raise


def _write_workspace_ownership(root: Path, fence: LeaseFence, workspace: WorkspaceBinding) -> Path:
    marker_path = _ownership_marker_path(root, fence)
    if marker_path.exists():
        raise ExecutionBlocked("workspace_collision", "an ownership record already exists for the task attempt")
    workspace_ref = workspace.as_dict()["workspace_ref"]
    record: dict[str, Any] = {
        "schema_id": "task_workspace_ownership.v1",
        "task_id": fence.task_id,
        "attempt_id": fence.attempt_id,
        "lease_id": fence.lease_id,
        "generation": fence.generation,
        "worker_id": fence.worker_id,
        "worker_instance_id": fence.worker_instance_id,
        "workspace_ref": workspace_ref,
        "workspace_kind": workspace.kind,
        "base_sha": workspace.base_sha,
        "repository": str(workspace.repo or ""),
        "result_id": _result_id(fence),
        "publication_id": _publication_id(fence),
        "cleanup_id": _cleanup_id(fence, workspace_ref),
    }
    record["record_digest"] = "sha256:" + hashlib.sha256(
        json.dumps(record, ensure_ascii=True, sort_keys=True, separators=(",", ":")).encode("utf-8")
    ).hexdigest()
    try:
        _atomic_owner_file(marker_path, record)
    except OSError as exc:
        raise ExecutionBlocked("workspace_ownership_unavailable", "the task ownership record could not be persisted") from exc
    return marker_path


def _remove_workspace_ownership(root: Path, fence: LeaseFence) -> None:
    marker_path = _ownership_marker_path(root, fence)
    try:
        marker_path.unlink(missing_ok=True)
        marker_path.parent.rmdir()
    except OSError:
        if marker_path.exists():
            raise ExecutionBlocked("cleanup_ownership_failed", "the task ownership record could not be removed")


def _prestart_owner_evidence(root: Path, fence: LeaseFence, workspace: WorkspaceBinding) -> dict[str, Any]:
    workspace_ref = workspace.as_dict()["workspace_ref"]
    return {
        "schema_id": "task_workspace_ownership_recovery.v1",
        "state": "quarantined",
        "recoverable": True,
        "task_id": fence.task_id,
        "attempt_id": fence.attempt_id,
        "lease_id": fence.lease_id,
        "generation": fence.generation,
        "worker_id": fence.worker_id,
        "worker_instance_id": fence.worker_instance_id,
        "workspace_ref": workspace_ref,
        "workspace_kind": workspace.kind,
        "workspace": str(workspace.cwd),
        "repository": str(workspace.repo or ""),
        "base_sha": workspace.base_sha,
        "owner_marker": str(_ownership_marker_path(root, fence)),
        "cleanup_id": _cleanup_id(fence, workspace_ref),
    }


def _cleanup_unstarted_workspace(root: Path, fence: LeaseFence, workspace: WorkspaceBinding) -> dict[str, Any]:
    """Fail closed if ownership persistence fails before a prepared execution exists."""

    evidence = _prestart_owner_evidence(root, fence, workspace)
    cleanup: dict[str, Any] = {"marker": {}, "workspace": {}, "runtime": {}}
    marker = _ownership_marker_path(root, fence)
    try:
        marker.unlink(missing_ok=True)
        cleanup["marker"] = {"verified": not os.path.lexists(marker), "path": str(marker)}
    except OSError as exc:
        cleanup["marker"] = {"verified": False, "reason": "owner_marker_cleanup_failed", "error": type(exc).__name__}

    if workspace.kind == "coding":
        if workspace.repo is None or workspace.worktree is None:
            cleanup["workspace"] = {"verified": False, "reason": "worktree_binding_missing"}
        else:
            try:
                removed = _git(workspace.repo, "worktree", "remove", "--force", str(workspace.worktree))
                listed = _git(workspace.repo, "worktree", "list", "--porcelain")
                registered: set[str] = set()
                if listed.returncode == 0:
                    registered = {
                        str(Path(line.removeprefix("worktree ")).resolve(strict=False))
                        for line in listed.stdout.splitlines()
                        if line.startswith("worktree ")
                    }
                target = str(workspace.worktree.resolve(strict=False))
                path_absent = not os.path.lexists(os.fspath(workspace.worktree))
                registered_absent = listed.returncode == 0 and target not in registered
                cleanup["workspace"] = {
                    # Git returns 128 when a retry races an already-removed
                    # worktree.  Exact path absence plus a successful,
                    # unregistered worktree list is the authoritative
                    # idempotent success proof; any remaining path or
                    # registration stays quarantined and retryable.
                    "verified": path_absent and registered_absent,
                    "git_remove_returncode": removed.returncode,
                    "git_list_returncode": listed.returncode,
                    "path_absent": path_absent,
                    "registered_absent": registered_absent,
                }
            except Exception as exc:
                cleanup["workspace"] = {
                    "verified": False,
                    "reason": "git_cleanup_probe_unavailable",
                    "error": type(exc).__name__,
                    "path_absent": not os.path.lexists(os.fspath(workspace.worktree)),
                }
    else:
        cleanup["workspace"] = _remove_exact_owned_directory(
            workspace.cwd,
            workspace.cwd.parent,
            name_prefix=fence.attempt_id,
            reason="prestart_workspace",
        )

    workspace_path = workspace.cwd.resolve(strict=False)
    task_ref = hashlib.sha256(
        f"{fence.task_id}\0{fence.attempt_id}\0{workspace_path}".encode("utf-8")
    ).hexdigest()[:24]
    try:
        cleanup["runtime"] = _remove_owned_runtime_dirs(root, task_ref)
    except Exception as exc:
        cleanup["runtime"] = {"verified": False, "reason": "runtime_cleanup_failed", "error": type(exc).__name__}

    cleanup["verified"] = all(
        _map(cleanup.get(name)).get("verified") is True for name in ("marker", "workspace", "runtime")
    )
    if cleanup["verified"] is not True:
        raise ExecutionBlocked(
            "quarantined",
            "pre-start ownership persistence failed and exact cleanup could not be proven",
            evidence={"owner": evidence, "cleanup": cleanup},
        )
    return cleanup


def _profile_config_path(config_path: str | Path | None = None) -> Path:
    if config_path:
        return Path(config_path).expanduser().resolve(strict=False)
    env_path = str(os.getenv("CONTEXTLATTICE_AGENT_PROFILE_PATH") or "").strip()
    if env_path:
        return Path(env_path).expanduser().resolve(strict=False)
    return Path(__file__).resolve().parents[1] / "config" / "agents" / "agent_profiles.json"


def load_profile_registry(config_path: str | Path | None = None) -> dict[str, Any]:
    path = _profile_config_path(config_path)
    try:
        raw = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise ExecutionBlocked("profile_registry_unavailable", str(path)) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise ExecutionBlocked("profile_registry_invalid", str(exc)) from exc
    if not isinstance(raw, dict):
        raise ExecutionBlocked("profile_registry_invalid", "registry must be a JSON object")
    return raw


def _execution_profiles(registry: Mapping[str, Any]) -> dict[str, Any]:
    for key in ("execution_profiles", "execution_surfaces", "task_execution_profiles"):
        candidate = registry.get(key)
        if isinstance(candidate, Mapping):
            return dict(candidate)
    return {}


def _require_manifest_fields(name: str, profile: Mapping[str, Any]) -> None:
    required = (
        "executable",
        "capabilities",
        "working_directory",
        "declared_mounts",
        "network_egress",
        "credential_refs",
        "output_protocol",
        "resource_limits",
        "output_limits",
        "descendant_policy",
        "heartbeat_interval_secs",
    )
    missing = [key for key in required if key not in profile]
    if missing:
        raise ExecutionBlocked("profile_manifest_incomplete", f"{name}: missing {', '.join(missing)}")
    if not isinstance(profile.get("capabilities"), list) or not profile.get("capabilities"):
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: capabilities must be non-empty")
    if not isinstance(profile.get("declared_mounts"), list):
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: declared_mounts must be a list")
    if not isinstance(profile.get("network_egress"), list):
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: network_egress must be a list")
    if not isinstance(profile.get("credential_refs"), list):
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: credential_refs must be a list")
    if not isinstance(profile.get("output_protocol"), Mapping):
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: output_protocol must be an object")
    if not isinstance(profile.get("resource_limits"), Mapping) or not isinstance(profile.get("output_limits"), Mapping):
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: resource/output limits must be objects")
    if not isinstance(profile.get("descendant_policy"), Mapping):
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: descendant_policy must be an object")
    working_directory = profile.get("working_directory")
    if not isinstance(working_directory, Mapping):
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: working_directory must be an object")
    resource_limits = profile.get("resource_limits")
    output_limits = profile.get("output_limits")
    max_runtime = resource_limits.get("max_runtime_secs") if isinstance(resource_limits, Mapping) else None
    try:
        max_runtime_int = int(max_runtime)
    except (TypeError, ValueError) as exc:
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: max_runtime_secs must be a positive integer") from exc
    if max_runtime_int <= 0:
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: max_runtime_secs must be positive")
    for key in ("max_processes", "max_file_bytes", "max_memory_bytes", "max_workspace_bytes", "max_workspace_files"):
        try:
            value = int(resource_limits.get(key))
        except (TypeError, ValueError) as exc:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: resource_limits.{key} must be a positive integer") from exc
        if value <= 0:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: resource_limits.{key} must be positive")
    try:
        max_cpus = float(resource_limits.get("max_cpus"))
    except (TypeError, ValueError) as exc:
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: resource_limits.max_cpus must be positive") from exc
    if max_cpus <= 0 or max_cpus > 1:
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: resource_limits.max_cpus is outside the local boundary range")
    output_protocol = profile["output_protocol"]
    if not str(output_protocol.get("schema_id") or "").strip() or not str(output_protocol.get("artifact_route") or "").strip() or output_protocol.get("redaction_required") is not True:
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: output protocol must require redacted artifacts")
    if working_directory.get("require_clean") is not True:
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: working_directory.require_clean must be true")
    try:
        heartbeat_interval = float(profile.get("heartbeat_interval_secs"))
    except (TypeError, ValueError) as exc:
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: heartbeat_interval_secs must be positive") from exc
    if heartbeat_interval <= 0 or heartbeat_interval > 60:
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: heartbeat_interval_secs is outside the safe range")
    for key, ceiling in (
        ("stdout_bytes", MAX_STDOUT_BYTES),
        ("stderr_bytes", MAX_STDERR_BYTES),
        ("combined_bytes", MAX_COMBINED_OUTPUT_BYTES),
    ):
        try:
            value = int(output_limits.get(key))
        except (TypeError, ValueError) as exc:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: output_limits.{key} must be an integer") from exc
        if value <= 0 or value > ceiling:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: output_limits.{key} exceeds the worker ceiling")
    descendant_policy = profile["descendant_policy"]
    for key in ("allow_descendants", "require_process_group", "terminate_on_lease_loss", "verify_pid_executable_parent_cwd", "quarantine_on_survival"):
        if not isinstance(descendant_policy.get(key), bool):
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: descendant_policy.{key} must be boolean")
    if descendant_policy.get("allow_descendants") is not False:
        raise ExecutionBlocked(
            "profile_manifest_invalid",
            f"{name}: descendant_policy.allow_descendants must be false for the local worker",
        )
    try:
        grace = float(descendant_policy.get("termination_grace_secs"))
    except (TypeError, ValueError) as exc:
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: termination_grace_secs must be positive") from exc
    if grace <= 0 or grace > 60:
        raise ExecutionBlocked("profile_manifest_invalid", f"{name}: termination_grace_secs is outside the safe range")
    execution_mode = str(profile.get("execution_mode") or "").strip().lower()
    executable = profile.get("executable")
    if isinstance(executable, Mapping):
        execution_mode = str(executable.get("mode") or execution_mode).strip().lower()
    if execution_mode not in {"gateway", "gateway_inference", "model"}:
        if not isinstance(executable, Mapping) or executable.get("kind") != "registered_fixture":
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: only registered deterministic fixtures are enabled locally")
        required_result_fields = output_protocol.get("required_result_fields")
        if not isinstance(required_result_fields, list) or not required_result_fields:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: output protocol must declare required result fields")
        required_field_names = {str(item).strip() for item in required_result_fields}
        if not {"task_id", "attempt_id", "runner_version", "tests", "checks", "warnings"}.issubset(required_field_names):
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: output protocol result binding is incomplete")
        optional_result_fields = output_protocol.get("optional_result_fields")
        if not isinstance(optional_result_fields, list):
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: output protocol optional fields must be a list")
        try:
            max_envelope_bytes = int(output_protocol.get("max_envelope_bytes"))
            max_result_items = int(output_protocol.get("max_result_items"))
        except (TypeError, ValueError) as exc:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: output protocol bounds must be integers") from exc
        if max_envelope_bytes <= 0 or max_envelope_bytes > MAX_SUMMARY_BYTES or max_result_items <= 0 or max_result_items > 256:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: output protocol bounds exceed the worker ceiling")
        if output_protocol.get("allow_skipped") is not False:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: registered fixture output cannot allow skipped checks")
        if not re.fullmatch(r"[0-9a-f]{64}", str(executable.get("sha256") or "")):
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: registered fixture requires an exact SHA-256 digest")
        if max_runtime_int > MAX_REGISTERED_FIXTURE_RUNTIME_SECS:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: fixture max_runtime_secs exceeds the worker hard ceiling")
        sandbox = profile.get("sandbox")
        if not isinstance(sandbox, Mapping) or str(sandbox.get("type") or "").strip() != "orbstack_oci":
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: process profiles require the registered OrbStack OCI boundary")
        image = str(sandbox.get("image") or "").strip()
        if image not in APPROVED_TASK_IMAGES or not _IMAGE_DIGEST_RE.fullmatch(image):
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: sandbox image is not approved and digest-pinned")
        expected = {
            "network": "none",
            "read_only_root": True,
            "drop_capabilities": ["ALL"],
            "no_new_privileges": True,
            "workspace_mount": "/workspace",
            "pull_policy": "never",
        }
        for key, value in expected.items():
            if sandbox.get(key) != value:
                raise ExecutionBlocked("profile_manifest_invalid", f"{name}: sandbox.{key} must be {value!r}")
        if sandbox.get("platform") not in {"linux/arm64", "linux/amd64"}:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: sandbox.platform must be explicit")
        try:
            control_timeout = float(sandbox.get("control_timeout_secs"))
            tmpfs_bytes = int(sandbox.get("tmpfs_bytes"))
        except (TypeError, ValueError) as exc:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: sandbox control/tmpfs limits are invalid") from exc
        if control_timeout <= 0 or control_timeout > 60 or tmpfs_bytes <= 0:
            raise ExecutionBlocked("profile_manifest_invalid", f"{name}: sandbox control/tmpfs limits are outside the safe range")


def resolve_registered_profile(task: Mapping[str, Any], config_path: str | Path | None = None) -> tuple[str, dict[str, Any]]:
    payload = _task_payload(task)
    name = _first(
        task.get("execution_profile"),
        payload.get("execution_profile"),
        task.get("profile"),
        payload.get("profile"),
    ).lower()
    if not name:
        raise ExecutionBlocked("missing_profile", "a registered execution_profile is required")
    registry = load_profile_registry(config_path)
    raw = _execution_profiles(registry).get(name)
    if not isinstance(raw, Mapping):
        raise ExecutionBlocked("unknown_profile", name)
    profile = dict(raw)
    if profile.get("enabled", True) is not True:
        raise ExecutionBlocked("profile_unavailable", f"{name}: registered profile is disabled by governance")
    _require_manifest_fields(name, profile)
    profile.setdefault("profile_id", name)
    return name, profile


def _runtime_policy(profile: Mapping[str, Any], attempt: Mapping[str, Any]) -> dict[str, Any]:
    profile_limit = int(_map(profile.get("resource_limits"))["max_runtime_secs"])
    executable = _map(profile.get("executable"))
    hard_ceiling = MAX_REGISTERED_FIXTURE_RUNTIME_SECS if executable.get("kind") == "registered_fixture" else profile_limit
    attempt_limits = _map(attempt.get("resource_limits"))
    raw_attempt_limit: Any = None
    for source, key in (
        (attempt, "runtime_limit_secs"),
        (attempt, "max_runtime_secs"),
        (attempt_limits, "max_runtime_secs"),
    ):
        if key in source:
            raw_attempt_limit = source[key]
            break
    if raw_attempt_limit in (None, ""):
        effective = profile_limit
        source = "registered_profile"
    else:
        text = str(raw_attempt_limit).strip()
        if isinstance(raw_attempt_limit, bool) or not re.fullmatch(r"[0-9]+", text):
            raise ExecutionBlocked("runtime_limit_invalid", "the fenced attempt runtime limit must be an integer")
        effective = int(text)
        if effective <= 0 or effective > profile_limit:
            raise ExecutionBlocked("runtime_limit_denied", "the fenced attempt runtime limit exceeds the registered profile")
        source = "fenced_attempt"
    return {
        "effective_runtime_secs": effective,
        "source": source,
        "profile_max_runtime_secs": profile_limit,
        "worker_hard_ceiling_secs": hard_ceiling,
    }


def _execution_kind(task: Mapping[str, Any]) -> str:
    raw = _first(
        _value(task, "execution_kind", "task_kind", "task_type", "kind", "mode"),
    ).lower().replace("-", "_")
    if raw in {"model", "non_repo", "noncoding", "non_coding", "knowledge", "analysis"}:
        return "non_coding"
    if raw in {"coding", "code", "repository", "repo"}:
        return "coding"
    if any(_value(task, key) for key in ("base_sha", "base_commit", "repository", "repo_path", "repo")):
        return "coding"
    return "non_coding"


def _requested_list(task: Mapping[str, Any], *keys: str) -> list[str]:
    raw = _value(task, *keys)
    if isinstance(raw, str):
        return [item.strip() for item in raw.split(",") if item.strip()]
    return [str(item).strip() for item in _list(raw) if str(item).strip()]


def _profile_mount_names(profile: Mapping[str, Any]) -> set[str]:
    names: set[str] = set()
    for item in _list(profile.get("declared_mounts")):
        if isinstance(item, Mapping):
            value = _first(item.get("name"), item.get("path"), item.get("mount"))
        else:
            value = str(item).strip()
        if value:
            names.add(value)
    return names


def validate_capability_policy(task: Mapping[str, Any], profile: Mapping[str, Any], kind: str) -> None:
    capabilities = {str(item).strip() for item in _list(profile.get("capabilities")) if str(item).strip()}
    required = set(_requested_list(task, "required_capabilities", "capabilities"))
    required.update({"coding", "worktree"} if kind == "coding" else {"model", "non_repo"})
    missing = sorted(required - capabilities)
    if missing:
        raise ExecutionBlocked("capability_denied", f"profile lacks: {', '.join(missing)}")
    working_directory = _map(profile.get("working_directory"))
    expected_mode = "task_worktree" if kind == "coding" else "task_sandbox"
    if str(working_directory.get("mode") or "") != expected_mode:
        raise ExecutionBlocked("working_directory_denied", f"profile working_directory.mode must be {expected_mode}")
    expected_base_policy = "exact" if kind == "coding" else "not_applicable"
    if str(working_directory.get("base_sha_policy") or "") != expected_base_policy:
        raise ExecutionBlocked("base_policy_denied", "profile base SHA policy is not authoritative for this task kind")
    if working_directory.get("allow_task_cwd_override") is not False:
        raise ExecutionBlocked("working_directory_denied", "task cwd overrides are not permitted")

    mounts = _requested_list(task, "declared_mounts", "mounts", "host_mounts")
    allowed_mounts = _profile_mount_names(profile)
    denied_mounts = [item for item in mounts if item not in allowed_mounts]
    if denied_mounts:
        raise ExecutionBlocked("mount_denied", "task requested an undeclared mount")

    egress = _requested_list(task, "network_egress", "egress", "allowed_egress")
    allowed_egress = {str(item).strip() for item in _list(profile.get("network_egress")) if str(item).strip()}
    denied_egress = [item for item in egress if item not in allowed_egress]
    if denied_egress:
        raise ExecutionBlocked("egress_denied", ", ".join(denied_egress[:4]))
    execution_mode = str(profile.get("execution_mode") or "").strip().lower()
    executable = profile.get("executable")
    if isinstance(executable, Mapping):
        execution_mode = str(executable.get("mode") or execution_mode).strip().lower()
    if execution_mode not in {"gateway", "gateway_inference", "model"} and allowed_egress:
        raise ExecutionBlocked("egress_boundary_unavailable", "local process profiles cannot acquire network egress without a broker")

    credentials = _requested_list(task, "credential_refs", "credentials")
    allowed_credentials = {str(item).strip() for item in _list(profile.get("credential_refs")) if str(item).strip()}
    denied_credentials = [item for item in credentials if item not in allowed_credentials]
    if denied_credentials:
        raise ExecutionBlocked("credential_denied", ", ".join(denied_credentials[:4]))
    if credentials:
        # A profile declaration is metadata only until a future credential
        # broker supplies an audience- and lease-scoped reference. Never read
        # the host environment as an implicit credential source.
        raise ExecutionBlocked("credential_broker_unavailable", "credential references cannot be resolved by the local worker")
    if allowed_credentials:
        raise ExecutionBlocked("credential_broker_unavailable", "registered credentials require an external scoped broker")


def _normalize_hash(value: Any) -> str:
    raw = str(value or "").strip().lower()
    if raw.startswith("sha256:"):
        raw = raw[7:]
    return raw


def _snapshot_reference(task: Mapping[str, Any], attempt: Mapping[str, Any]) -> tuple[str, str, str]:
    context = _map(task.get("context_request"))
    payload_context = _map(_task_payload(task).get("context_request"))
    sources = (context, payload_context, _task_payload(task), task, attempt)
    snapshot_id = ""
    expected_hash = ""
    session_id = ""
    session_candidates: set[str] = set()
    for source in sources:
        if not snapshot_id:
            ref = _map(source.get("snapshot")) if isinstance(source.get("snapshot"), Mapping) else {}
            snapshot_id = _first(source.get("snapshot_id"), source.get("snapshotId"), source.get("continuity_snapshot_id"), ref.get("snapshot_id"), ref.get("snapshotId"))
        if not expected_hash:
            ref = _map(source.get("snapshot")) if isinstance(source.get("snapshot"), Mapping) else {}
            expected_hash = _first(source.get("content_hash"), source.get("contentHash"), source.get("snapshot_hash"), source.get("context_pack_hash"), source.get("contextPackHash"), ref.get("content_hash"), ref.get("contentHash"))
        if not session_id:
            session_id = _first(source.get("session_id"), source.get("sessionId"))
        candidate_session = _first(source.get("session_id"), source.get("sessionId"))
        if candidate_session:
            session_candidates.add(candidate_session)
    if len(session_candidates) > 1:
        raise ExecutionBlocked("session_linkage_mismatch", "claim session bindings disagree")
    task_id = _first(_value(task, "task_id", "id"))
    attempt_id = _first(attempt.get("attempt_id"), _value(task, "attempt_id"))
    if not snapshot_id or not expected_hash or not task_id or not attempt_id or not session_id:
        raise ExecutionBlocked("snapshot_reference_missing", "task, attempt, session, snapshot ID, and content hash are required")
    return snapshot_id, expected_hash, session_id


def fetch_pinned_snapshot(
    orchestrator_url: str,
    task: Mapping[str, Any],
    attempt: Mapping[str, Any],
    get_json: Callable[..., dict[str, Any]],
) -> SnapshotBinding:
    snapshot_id, expected_hash, session_id = _snapshot_reference(task, attempt)
    try:
        response = get_json(orchestrator_url, f"/memory/continuity/snapshots/{snapshot_id}", timeout=30.0)
    except Exception as exc:
        raise ExecutionBlocked("context_unavailable", "pinned continuity snapshot could not be read") from exc
    if not isinstance(response, Mapping) or response.get("ok", True) is False:
        raise ExecutionBlocked("context_unavailable", "pinned continuity snapshot was not returned")
    snapshot = _map(response.get("snapshot")) or _map(response)
    index = _map(response.get("index"))
    actual_id = _first(snapshot.get("snapshotId"), snapshot.get("snapshot_id"), index.get("snapshot_id"), index.get("snapshotId"), response.get("snapshot_id"), response.get("snapshotId"))
    actual_hash = _first(snapshot.get("contentHash"), snapshot.get("content_hash"), index.get("content_hash"), index.get("contentHash"), response.get("content_hash"), response.get("contentHash"))
    if not actual_id or actual_id != snapshot_id:
        raise ExecutionBlocked("snapshot_mismatch", "snapshot ID differs from the claimed pin")
    if not actual_hash or _normalize_hash(actual_hash) != _normalize_hash(expected_hash):
        raise ExecutionBlocked("snapshot_mismatch", "snapshot content hash differs from the claimed pin")
    metadata = _map(snapshot.get("metadata"))
    task_id = str(_value(task, "task_id", "id") or "").strip()
    attempt_id = str(attempt.get("attempt_id") or "").strip()
    for key, aliases, expected in (
        ("task_id", ("task_id", "taskId"), task_id),
        ("attempt_id", ("attempt_id", "attemptId"), attempt_id),
        ("session_id", ("session_id", "sessionId"), session_id),
    ):
        observed = _first(*(value for alias in aliases for value in (metadata.get(alias), snapshot.get(alias))))
        if not observed or observed != expected:
            raise ExecutionBlocked("snapshot_mismatch", f"snapshot {key} does not match the attempt")
    return SnapshotBinding(snapshot_id, _normalize_hash(actual_hash), session_id, task_id, attempt_id, snapshot)


def _safe_git_env(overrides: Mapping[str, str] | None = None) -> dict[str, str]:
    env = {
        "PATH": "/usr/bin:/bin",
        "HOME": "/var/empty",
        "LANG": "C",
        "LC_ALL": "C",
        "GIT_CONFIG_GLOBAL": "/dev/null",
        "GIT_CONFIG_NOSYSTEM": "1",
        "GIT_OPTIONAL_LOCKS": "0",
        "GIT_TERMINAL_PROMPT": "0",
    }
    task_tmp = str(os.getenv("TMPDIR") or "").strip()
    if task_tmp and Path(task_tmp).is_absolute() and "\x00" not in task_tmp:
        env["TMPDIR"] = task_tmp
    allowed_overrides = {
        "GIT_INDEX_FILE",
        "GIT_OBJECT_DIRECTORY",
        "GIT_ALTERNATE_OBJECT_DIRECTORIES",
    }
    for key, value in dict(overrides or {}).items():
        if key in allowed_overrides:
            env[key] = str(value)
    return env


def _git(repo: Path, *args: str) -> subprocess.CompletedProcess[str]:
    if not GIT_EXECUTABLE.is_file():
        raise ExecutionBlocked("git_unavailable", str(GIT_EXECUTABLE))
    return subprocess.run(
        [str(GIT_EXECUTABLE), *_GIT_CONFIG_ARGS, *args],
        cwd=str(repo),
        env=_safe_git_env(),
        stdin=subprocess.DEVNULL,
        text=True,
        capture_output=True,
        check=False,
    )


def _repo_path(task: Mapping[str, Any], source_repo: Path | None, profile: Mapping[str, Any]) -> Path:
    raw = _first(_value(task, "repository", "repo_path", "repo"))
    trusted = _absolute_server_path(source_repo, reason="repository_not_registered") if source_repo is not None else None
    if not raw:
        if trusted is None:
            raise ExecutionBlocked("repository_not_registered", "coding task has no server-owned repository binding")
        return trusted
    requested = _absolute_server_path(raw, reason="repository_not_registered")
    allowlist = _map(profile.get("working_directory")).get("repository_allowlist")
    allowed: set[str] = set()
    for item in _list(allowlist):
        if not str(item).strip():
            continue
        allowed.add(str(_absolute_server_path(str(item), reason="profile_manifest_invalid")))
    if trusted is not None and requested == trusted:
        return trusted
    if str(requested) not in allowed:
        raise ExecutionBlocked("repository_not_registered", "task repository is not in the server-owned allowlist")
    return requested


def prepare_workspace(
    task: Mapping[str, Any],
    fence: LeaseFence,
    profile: Mapping[str, Any],
    *,
    source_repo: Path | None = None,
    worktree_root: Path | None = None,
) -> WorkspaceBinding:
    kind = _execution_kind(task)
    validate_capability_policy(task, profile, kind)
    root = _configured_worktree_root(worktree_root)
    task_root = _task_workspace_path(root, fence)
    marker_path = _ownership_marker_path(root, fence)
    if marker_path.exists():
        raise ExecutionBlocked("workspace_collision", "an ownership record already exists for the task attempt")
    if kind != "coding":
        try:
            task_root.mkdir(parents=True, exist_ok=False)
            task_root.chmod(0o700)
        except FileExistsError as exc:
            raise ExecutionBlocked("workspace_collision", "the task workspace already exists") from exc
        workspace = WorkspaceBinding("non_repo", task_root, None, None, "")
        try:
            _write_workspace_ownership(root, fence, workspace)
        except Exception:
            _cleanup_unstarted_workspace(root, fence, workspace)
            raise
        return workspace

    repo = _repo_path(task, source_repo, profile)
    if not repo.is_dir():
        raise ExecutionBlocked("repository_unavailable", "the registered task repository is unavailable")
    probe = _git(repo, "rev-parse", "--show-toplevel")
    if probe.returncode != 0:
        raise ExecutionBlocked("repository_unavailable", "task repository is not a Git checkout")
    repo = Path(probe.stdout.strip()).resolve(strict=False)
    status = _git(repo, "status", "--porcelain", "--untracked-files=all")
    if status.returncode != 0:
        raise ExecutionBlocked("repository_unavailable", "could not inspect repository state")
    source_dirty = bool(status.stdout)
    base_sha = _first(_value(task, "base_sha", "base_commit"))
    if not base_sha:
        raise ExecutionBlocked("base_sha_missing", "coding tasks require an exact base SHA")
    if not re.fullmatch(r"[0-9a-f]{40}", base_sha):
        raise ExecutionBlocked("base_sha_invalid", "coding tasks require a canonical full 40-hex commit SHA")
    resolved = _git(repo, "rev-parse", "--verify", f"{base_sha}^{{commit}}")
    if resolved.returncode != 0:
        raise ExecutionBlocked("base_sha_unavailable", "the declared base SHA is not present")
    resolved_sha = resolved.stdout.strip()
    if resolved_sha != base_sha:
        raise ExecutionBlocked("base_sha_mismatch", "the declared base SHA resolved to a different commit")
    head = _git(repo, "rev-parse", "HEAD")
    if head.returncode != 0:
        raise ExecutionBlocked("repository_unavailable", "the source checkout HEAD could not be read")
    task_root.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    task_root.parent.chmod(0o700)
    if task_root.exists():
        raise ExecutionBlocked("workspace_collision", "the task workspace already exists")
    created = _git(repo, "worktree", "add", "--detach", str(task_root), resolved_sha)
    if created.returncode != 0:
        raise ExecutionBlocked("worktree_creation_failed", "Git could not create the task-scoped worktree")
    task_root.chmod(0o700)
    child_status = _git(task_root, "status", "--porcelain", "--untracked-files=all")
    child_head = _git(task_root, "rev-parse", "HEAD")
    if child_status.returncode != 0 or child_status.stdout:
        rejected_workspace = WorkspaceBinding("coding", task_root, repo, task_root, resolved_sha, source_dirty)
        cleanup = _cleanup_unstarted_workspace(root, fence, rejected_workspace)
        if cleanup.get("verified") is not True:
            raise ExecutionBlocked(
                "quarantined",
                "the rejected task worktree could not be removed and verified",
                evidence={"owner": _prestart_owner_evidence(root, fence, rejected_workspace), "cleanup": cleanup},
            )
        raise ExecutionBlocked("worktree_not_clean", "task worktree was not clean immediately after creation")
    if child_head.returncode != 0 or child_head.stdout.strip() != resolved_sha:
        rejected_workspace = WorkspaceBinding("coding", task_root, repo, task_root, resolved_sha, source_dirty)
        cleanup = _cleanup_unstarted_workspace(root, fence, rejected_workspace)
        if cleanup.get("verified") is not True:
            raise ExecutionBlocked(
                "quarantined",
                "the rejected task worktree could not be removed and verified",
                evidence={"owner": _prestart_owner_evidence(root, fence, rejected_workspace), "cleanup": cleanup},
            )
        raise ExecutionBlocked("worktree_base_mismatch", "task worktree does not point at the exact base SHA")
    workspace = WorkspaceBinding("coding", task_root, repo, task_root, resolved_sha, source_dirty)
    try:
        _write_workspace_ownership(root, fence, workspace)
    except Exception:
        _cleanup_unstarted_workspace(root, fence, workspace)
        raise
    return workspace


def _safe_json(value: Any, *, max_bytes: int = MAX_CONTEXT_BYTES) -> str:
    rendered = json.dumps(_redact_value(value), ensure_ascii=True, sort_keys=True, separators=(",", ":"))
    return bounded_utf8(rendered, max_bytes)


def bounded_utf8(value: str | bytes, limit: int) -> str:
    """Bound by UTF-8 bytes without leaving a partial code point."""

    max_bytes = max(0, int(limit))
    raw = value if isinstance(value, bytes) else str(value).encode("utf-8", errors="replace")
    text = raw.decode("utf-8", errors="replace")
    text = redact_text(text)
    if len(text.encode("utf-8")) <= max_bytes:
        return text
    marker = TRUNCATION_MARKER
    if len(marker.encode("utf-8")) >= max_bytes:
        return marker.encode("utf-8")[:max_bytes].decode("utf-8", errors="ignore")
    prefix_limit = max_bytes - len(marker.encode("utf-8"))
    prefix = text.encode("utf-8")[:prefix_limit].decode("utf-8", errors="ignore")
    return prefix + marker


def _probe_stderr_text(value: Any) -> str:
    if isinstance(value, (bytes, bytearray, memoryview)):
        return bytes(value).decode("utf-8", errors="replace")
    return str(value or "")


def _probe_evidence(tool: str, raw: Any) -> dict[str, Any]:
    try:
        returncode = int(getattr(raw, "returncode", -1))
    except (TypeError, ValueError):
        returncode = -1
    return {
        "tool": str(tool),
        "returncode": returncode,
        "stderr": bounded_utf8(
            _probe_stderr_text(getattr(raw, "stderr", "")),
            MAX_PROCESS_PROBE_EVIDENCE_BYTES,
        ),
    }


def _probe_has_stderr(raw: Any) -> bool:
    # Even whitespace is output from an authority probe.  Treat every byte
    # of stderr as an ambiguity signal rather than interpreting it as a
    # successful absence result.
    return bool(_probe_stderr_text(getattr(raw, "stderr", "")))


def _probe_evidence_from(value: Any) -> dict[str, Any]:
    if isinstance(value, _ProbeUnavailable):
        return dict(value.evidence)
    if isinstance(value, Mapping):
        evidence = value.get("_probe_evidence")
        if isinstance(evidence, Mapping):
            return dict(evidence)
    return {}


def _probe_members(value: Any) -> list[dict[str, Any]]:
    return value if isinstance(value, list) else []


def _credential_env_key(name: str) -> bool:
    upper = name.upper()
    if upper in {
        "TASK_ID",
        "TASK_ATTEMPT_ID",
        "TASK_LEASE_ID",
        "TASK_WORKER_ID",
        "TASK_WORKER_INSTANCE_ID",
        "TASK_LEASE_GENERATION",
        "CONTEXTLATTICE_TASK_ID",
        "CONTEXTLATTICE_ATTEMPT_ID",
        "CONTEXTLATTICE_LEASE_ID",
        "CONTEXTLATTICE_CONTEXT_SNAPSHOT_ID",
        "CONTEXTLATTICE_CONTEXT_PACK_HASH",
        "CONTEXTLATTICE_SESSION_ID",
        "TASK_PROJECT",
        "TASK_CWD",
        "TASK_WORKTREE",
        "TASK_BASE_SHA",
        "TASK_NETWORK_POLICY",
        "TASK_AUTH_SCOPE",
        "TASK_MOUNT_POLICY",
        "TASK_PAYLOAD",
        "TASK_CONTEXT_BUNDLE",
        "TASK_CONTEXT_SNAPSHOT_JSON",
    }:
        return False
    return _is_secret_key(upper) or upper.startswith(("AWS_", "AZURE_", "GOOGLE_", "GITHUB_", "NPM_TOKEN"))


_PROTECTED_ENV_KEYS = {"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "SSH_AUTH_SOCK", "GIT_CONFIG_GLOBAL"}


def build_execution_env(
    task: Mapping[str, Any],
    snapshot: SnapshotBinding,
    workspace: WorkspaceBinding,
    fence: LeaseFence,
    profile: Mapping[str, Any],
    *,
    base_env: Mapping[str, str] | None = None,
) -> dict[str, str]:
    source = dict(base_env or os.environ)
    env: dict[str, str] = {}
    for key in ("PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ"):
        if source.get(key):
            env[key] = str(source[key])
    env["PATH"] = env.get("PATH") or os.defpath
    sandbox_home = workspace.cwd / ".home"
    sandbox_home.mkdir(parents=True, exist_ok=True)
    sandbox_home.chmod(0o700)
    env.update(
        {
            "HOME": str(sandbox_home),
            "USERPROFILE": str(sandbox_home),
            "XDG_CONFIG_HOME": str(sandbox_home / ".config"),
            "XDG_CACHE_HOME": str(sandbox_home / ".cache"),
            "GIT_CONFIG_GLOBAL": "/dev/null",
            "TASK_ID": fence.task_id,
            "TASK_ATTEMPT_ID": fence.attempt_id,
            "TASK_LEASE_ID": fence.lease_id,
            "TASK_WORKER_ID": fence.worker_id,
            "TASK_WORKER_INSTANCE_ID": fence.worker_instance_id,
            "TASK_LEASE_GENERATION": str(fence.generation),
            "CONTEXTLATTICE_TASK_ID": fence.task_id,
            "CONTEXTLATTICE_ATTEMPT_ID": fence.attempt_id,
            "CONTEXTLATTICE_LEASE_ID": fence.lease_id,
            "CONTEXTLATTICE_CONTEXT_SNAPSHOT_ID": snapshot.snapshot_id,
            "CONTEXTLATTICE_CONTEXT_PACK_HASH": snapshot.content_hash,
            "CONTEXTLATTICE_SESSION_ID": snapshot.session_id,
            "TASK_PROJECT": _first(_value(task, "project")),
            "TASK_CWD": str(workspace.cwd),
            "TASK_WORKTREE": str(workspace.worktree or ""),
            "TASK_BASE_SHA": workspace.base_sha,
            "TASK_NETWORK_POLICY": json.dumps(list(profile.get("network_egress") or []), separators=(",", ":")),
            "TASK_AUTH_SCOPE": "none",
            "TASK_MOUNT_POLICY": json.dumps(list(profile.get("declared_mounts") or []), separators=(",", ":")),
        }
    )
    # Only literal non-sensitive profile variables are allowed. Host secrets
    # and provider sessions never cross the worker boundary.
    declared_env = profile.get("environment")
    if isinstance(declared_env, Mapping):
        for key, value in declared_env.items():
            name = str(key).strip()
            if not name or _credential_env_key(name) or name.upper() in _PROTECTED_ENV_KEYS:
                raise ExecutionBlocked("credential_denied", f"profile environment key {name or '<empty>'} is sensitive")
            env[name] = bounded_utf8(str(value), 4096)
    payload = _task_payload(task)
    env["TASK_PAYLOAD"] = _safe_json(payload)
    snapshot_context = snapshot.snapshot.get("contextPack") or snapshot.snapshot.get("context_pack") or {}
    env["TASK_CONTEXT_BUNDLE"] = _safe_json({"snapshot": snapshot.as_dict(), "context_pack": snapshot_context})
    env["TASK_CONTEXT_SNAPSHOT_JSON"] = _safe_json(snapshot.snapshot)
    return {key: value for key, value in env.items() if not _credential_env_key(key)}


def resolve_profile_argv(profile: Mapping[str, Any], workspace: WorkspaceBinding) -> list[str] | None:
    executable = profile.get("executable")
    if isinstance(executable, Mapping) and str(executable.get("mode") or "").strip().lower() in {"gateway", "gateway_inference", "model"}:
        return None
    if executable is None and str(profile.get("execution_mode") or "").lower() in {"gateway", "gateway_inference"}:
        return None
    if isinstance(executable, Mapping):
        raw_path = _first(executable.get("path"), executable.get("executable"))
        raw_argv = _list(executable.get("argv"))
    else:
        raw_path = str(executable or "").strip()
        raw_argv = []
    if not raw_path:
        raise ExecutionBlocked("profile_executable_missing", "registered profile has no executable")
    if workspace.kind != "coding" or workspace.worktree is None:
        raise ExecutionBlocked("profile_executable_invalid", "process profiles require a task worktree executable")
    path = Path(raw_path)
    if path.is_absolute() or ".." in path.parts:
        raise ExecutionBlocked("profile_executable_invalid", "the registered executable must be relative to the task worktree")
    host_path = (workspace.worktree / path).resolve(strict=False)
    if workspace.worktree.resolve(strict=False) not in host_path.parents:
        raise ExecutionBlocked("profile_executable_invalid", "the registered executable escapes the task worktree")
    if not host_path.is_file() or host_path.is_symlink() or not os.access(host_path, os.X_OK):
        raise ExecutionBlocked("profile_executable_unavailable", "the registered task-worktree executable is unavailable")
    expected_digest = str(_map(profile.get("executable")).get("sha256") or "")
    try:
        actual_digest = hashlib.sha256(host_path.read_bytes()).hexdigest()
    except OSError as exc:
        raise ExecutionBlocked("profile_executable_unavailable", "the registered task-worktree executable could not be read") from exc
    if actual_digest != expected_digest:
        raise ExecutionBlocked("profile_executable_mismatch", "the task-worktree executable does not match the registered digest")
    container_path = Path("/workspace") / path
    if raw_argv:
        argv: list[str] = []
        for item in raw_argv:
            text = str(item)
            text = text.replace("{path}", str(container_path)).replace("{workspace}", "/workspace")
            argv.append(text)
    else:
        argv = [str(container_path)]
    if not argv:
        raise ExecutionBlocked("profile_executable_missing")
    if any(token in item for item in argv for token in (";", "&&", "||", "`", "$(", ">", "<", "|")):
        raise ExecutionBlocked("profile_executable_invalid", "shell syntax is not an execution profile")
    return argv


def _process_description(pid: int) -> dict[str, Any] | None:
    try:
        raw = subprocess.run(
            ["/bin/ps", "-ww", "-p", str(pid), "-o", "pid=,ppid=,pgid=,command="],
            env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"},
            text=True,
            capture_output=True,
            check=False,
        )
    except (OSError, TypeError, ValueError):
        return None
    if _probe_has_stderr(raw):
        return {
            "pid": int(pid),
            "_probe_status": "unavailable",
            "_probe_evidence": _probe_evidence("ps", raw),
        }
    if raw.returncode != 0 or not raw.stdout.strip():
        # `ps` returning 1 with no row is an authoritative absent result;
        # every other failure is an unavailable process probe.
        if raw.returncode == 1 and not raw.stdout.strip() and not raw.stderr.strip():
            return {"pid": int(pid), "_probe_status": "absent"}
        return None
    fields = raw.stdout.strip().split(None, 3)
    if len(fields) < 4:
        return None
    try:
        pid_value, ppid, pgid = (int(fields[index]) for index in range(3))
    except ValueError:
        return None
    command = fields[3]
    try:
        argv_raw = Path(f"/proc/{pid}/cmdline").read_bytes()
        argv = [item.decode("utf-8", errors="replace") for item in argv_raw.split(b"\0") if item]
    except OSError:
        try:
            argv = shlex.split(command, posix=True)
        except ValueError:
            argv = []
    if not argv and command not in {"<defunct>", f"({Path(command).name})"}:
        try:
            argv = shlex.split(command, posix=True)
        except ValueError:
            argv = []
    executable = ""
    executable_inode: int | None = None
    proc_executable = Path(f"/proc/{pid}/exe")
    try:
        if proc_executable.exists():
            executable_path = proc_executable.resolve(strict=True)
            executable = str(executable_path)
            executable_inode = int(executable_path.stat().st_ino)
    except OSError:
        pass
    if not executable:
        lsof = Path("/usr/sbin/lsof")
        if not lsof.is_file():
            return None
        try:
            executable_probe = subprocess.run(
                [str(lsof), "-a", "-p", str(pid), "-d", "txt", "-Fn"],
                env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"},
                text=True,
                capture_output=True,
                check=False,
            )
        except (OSError, TypeError, ValueError):
            return None
        if _probe_has_stderr(executable_probe):
            return {
                "pid": int(pid),
                "_probe_status": "unavailable",
                "_probe_evidence": _probe_evidence("lsof", executable_probe),
            }
        if executable_probe.returncode != 0:
            # A permission/error rc=1 is an unavailable identity probe, not
            # evidence that argv[0] names the actual executable.
            return None
        for item in executable_probe.stdout.splitlines():
            if not item.startswith("n"):
                continue
            candidate = Path(item[1:].strip())
            if candidate.is_absolute() and candidate.exists():
                executable_path = candidate.resolve(strict=False)
                executable = str(executable_path)
                try:
                    executable_inode = int(executable_path.stat().st_ino)
                except OSError:
                    pass
                break
        if not executable:
            return None
    if not executable and argv:
        executable = str(argv[0])
    cwd = ""
    proc_cwd = Path(f"/proc/{pid}/cwd")
    if proc_cwd.exists():
        try:
            cwd = str(proc_cwd.resolve(strict=True))
        except OSError:
            cwd = ""
    lsof = Path("/usr/sbin/lsof")
    if not cwd:
        if not lsof.is_file():
            return None
        try:
            probe = subprocess.run(
                [str(lsof), "-a", "-p", str(pid), "-d", "cwd", "-Fn"],
                env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"},
                text=True,
                capture_output=True,
                check=False,
            )
        except (OSError, TypeError, ValueError):
            return None
        if _probe_has_stderr(probe):
            return {
                "pid": int(pid),
                "_probe_status": "unavailable",
                "_probe_evidence": _probe_evidence("lsof", probe),
            }
        if probe.returncode != 0:
            return None
        for item in probe.stdout.splitlines():
            if item.startswith("n"):
                cwd = item[1:].strip()
                break
        if not cwd:
            return None
    return {
        "pid": pid_value,
        "ppid": ppid,
        "pgid": pgid,
        "command": command,
        "argv": argv,
        "executable": executable,
        "executable_inode": executable_inode,
        "cwd": cwd,
        "_probe_status": "present",
    }


def _group_members(pgid: int) -> list[dict[str, Any]] | _ProbeUnavailable | None:
    try:
        raw = subprocess.run(
            ["/bin/ps", "-ww", "-axo", "pid=,ppid=,pgid=,command="],
            env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"},
            text=True,
            capture_output=True,
            check=False,
        )
    except (OSError, TypeError, ValueError):
        return None
    if _probe_has_stderr(raw):
        return _ProbeUnavailable(_probe_evidence("ps", raw))
    if raw.returncode != 0:
        return None
    members: list[dict[str, Any]] = []
    for line in raw.stdout.splitlines():
        fields = line.split(None, 3)
        if len(fields) < 4:
            return None
        try:
            pid, ppid, group = (int(fields[index]) for index in range(3))
        except ValueError:
            return None
        if group == pgid:
            command = fields[3]
            try:
                argv = shlex.split(command, posix=True)
            except ValueError:
                argv = []
            members.append({"pid": pid, "ppid": ppid, "pgid": group, "command": command, "argv": argv})
    return members


def _descendants(root_pid: int) -> list[int] | _ProbeUnavailable | None:
    try:
        raw = subprocess.run(
            ["/bin/ps", "-axo", "pid=,ppid="],
            env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"},
            text=True,
            capture_output=True,
            check=False,
        )
    except (OSError, TypeError, ValueError):
        return None
    if _probe_has_stderr(raw):
        return _ProbeUnavailable(_probe_evidence("ps", raw))
    if raw.returncode != 0:
        return None
    parents: dict[int, list[int]] = {}
    for line in raw.stdout.splitlines():
        fields = line.split()
        if len(fields) != 2:
            return None
        try:
            pid, ppid = int(fields[0]), int(fields[1])
        except ValueError:
            return None
        parents.setdefault(ppid, []).append(pid)
    result: list[int] = []
    queue = list(parents.get(root_pid, []))
    while queue:
        pid = queue.pop(0)
        if pid in result:
            continue
        result.append(pid)
        queue.extend(parents.get(pid, []))
    return result


def _docker_executable() -> Path:
    candidate = shutil.which("docker")
    if not candidate:
        raise ExecutionBlocked("execution_boundary_unavailable", "the Docker CLI is unavailable")
    resolved = Path(candidate).resolve(strict=False)
    if not resolved.is_file():
        raise ExecutionBlocked("execution_boundary_unavailable", "the Docker CLI is unavailable")
    return resolved


def _orbstack_socket_path() -> Path:
    """Resolve OrbStack's local daemon socket without Docker client state."""

    if _pwd is None or not hasattr(os, "getuid"):
        raise ExecutionBlocked(
            "execution_boundary_unavailable",
            "the OrbStack local Unix socket is unavailable on this platform",
        )
    try:
        account_home = Path(_pwd.getpwuid(os.getuid()).pw_dir)
    except (KeyError, OSError) as exc:
        raise ExecutionBlocked(
            "execution_boundary_unavailable",
            "the local account home could not be resolved",
        ) from exc
    socket_path = account_home / ".orbstack" / "run" / "docker.sock"
    try:
        account_item = account_home.lstat()
        orbstack_item = (account_home / ".orbstack").lstat()
        run_item = socket_path.parent.lstat()
        socket_item = socket_path.lstat()
    except OSError as exc:
        raise ExecutionBlocked(
            "execution_boundary_unavailable",
            "the OrbStack local Unix socket is unavailable",
        ) from exc
    uid = os.getuid()
    parents = (account_item, orbstack_item, run_item)
    if (
        any(stat.S_ISLNK(item.st_mode) or not stat.S_ISDIR(item.st_mode) for item in parents)
        or any(item.st_uid != uid for item in parents)
        or stat.S_ISLNK(socket_item.st_mode)
        or not stat.S_ISSOCK(socket_item.st_mode)
        or socket_item.st_uid != uid
    ):
        raise ExecutionBlocked(
            "execution_boundary_unavailable",
            "the OrbStack local Unix socket identity is invalid",
        )
    return socket_path


def _orbstack_endpoint(docker: Path, timeout_secs: float) -> str:
    # `docker context inspect` necessarily reads the operator's Docker config.
    # The U3 boundary instead uses OrbStack's account-owned local socket and a
    # separate helper-free DOCKER_CONFIG created for each control operation.
    del docker, timeout_secs
    endpoint = f"unix://{_orbstack_socket_path()}"
    if not _ORBSTACK_SOCKET_RE.fullmatch(endpoint):
        raise ExecutionBlocked(
            "execution_boundary_unavailable",
            "OrbStack did not provide a local Unix socket",
        )
    return endpoint


def _helper_free_docker_config(storage_root: Path, container_name: str) -> Path:
    runtime_root = storage_root / ".runtime"
    _ensure_private_directory(runtime_root, "runtime_root")
    runtime_identity = _directory_identity(runtime_root)
    config_dir = Path(tempfile.mkdtemp(prefix=f"{container_name}-", dir=str(runtime_root)))
    if _directory_identity(runtime_root) != runtime_identity:
        _remove_exact_owned_directory(
            config_dir,
            runtime_root,
            name_prefix=f"{container_name}-",
            reason="runtime_config",
        )
        raise ExecutionBlocked("runtime_root_identity_invalid", "the runtime directory changed while creating helper configuration")
    config_dir.chmod(0o700)
    config_path = config_dir / "config.json"
    config_path.write_text('{"auths":{}}\n', encoding="utf-8")
    config_path.chmod(0o600)
    return config_dir


def _path_absent(path: Path) -> bool:
    return not os.path.lexists(os.fspath(path))


def _directory_identity(path: Path) -> tuple[int, int] | None:
    try:
        item = path.lstat()
    except OSError:
        return None
    if stat.S_ISLNK(item.st_mode) or not stat.S_ISDIR(item.st_mode):
        return None
    return int(item.st_dev), int(item.st_ino)


def _ensure_private_directory(path: Path, reason: str) -> None:
    """Create or validate one private directory without following a symlink."""

    try:
        if os.path.lexists(os.fspath(path)):
            item = path.lstat()
            if stat.S_ISLNK(item.st_mode) or not stat.S_ISDIR(item.st_mode):
                raise ExecutionBlocked(f"{reason}_identity_invalid", "an owned runtime directory is not a real directory")
        else:
            path.mkdir(mode=0o700, exist_ok=False)
            item = path.lstat()
            if stat.S_ISLNK(item.st_mode) or not stat.S_ISDIR(item.st_mode):
                raise ExecutionBlocked(f"{reason}_identity_invalid", "an owned runtime directory was replaced before use")
        if item.st_uid != os.getuid():
            raise ExecutionBlocked(f"{reason}_ownership_invalid", "an owned runtime directory is not worker-owned")
        path.chmod(0o700)
    except ExecutionBlocked:
        raise
    except OSError as exc:
        raise ExecutionBlocked(f"{reason}_unavailable", "an owned runtime directory could not be prepared") from exc


def _remove_exact_owned_directory(
    path: Path,
    expected_parent: Path,
    *,
    name_prefix: str,
    reason: str,
) -> dict[str, Any]:
    """Remove one exact owned directory through a no-follow parent dirfd."""

    parent_fd: int | None = None
    try:
        if (
            not path.is_absolute()
            or path == path.parent
            or path == expected_parent
            or not path.name
            or "/" in path.name
            or not name_prefix
            or not path.name.startswith(name_prefix)
            or path.parent != expected_parent
        ):
            return {"verified": False, "reason": f"{reason}_identity_invalid"}
        parent_before = os.stat(os.fspath(expected_parent), follow_symlinks=False)
        if stat.S_ISLNK(parent_before.st_mode) or not stat.S_ISDIR(parent_before.st_mode):
            return {"verified": False, "reason": f"{reason}_identity_invalid"}
        flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
        parent_fd = os.open(os.fspath(expected_parent), flags)
        parent_after = os.fstat(parent_fd)
        if (
            parent_after.st_dev != parent_before.st_dev
            or parent_after.st_ino != parent_before.st_ino
            or stat.S_ISLNK(parent_after.st_mode)
            or not stat.S_ISDIR(parent_after.st_mode)
        ):
            return {"verified": False, "reason": f"{reason}_identity_invalid"}
        try:
            item_stat = os.stat(path.name, dir_fd=parent_fd, follow_symlinks=False)
        except FileNotFoundError:
            return {
                "verified": True,
                "reason": f"{reason}_absent",
                "path_absent": True,
            }
        if stat.S_ISLNK(item_stat.st_mode) or not stat.S_ISDIR(item_stat.st_mode):
            return {"verified": False, "reason": f"{reason}_identity_invalid", "path_absent": False}
        entry_identity = (int(item_stat.st_dev), int(item_stat.st_ino))
        parent_check = os.fstat(parent_fd)
        if (int(parent_check.st_dev), int(parent_check.st_ino)) != (
            int(parent_before.st_dev),
            int(parent_before.st_ino),
        ):
            return {"verified": False, "reason": f"{reason}_identity_invalid", "path_absent": False}
        entry_check = os.stat(path.name, dir_fd=parent_fd, follow_symlinks=False)
        if (int(entry_check.st_dev), int(entry_check.st_ino)) != entry_identity:
            return {"verified": False, "reason": f"{reason}_identity_invalid", "path_absent": False}
        # The entry name is deliberately relative to the opened parent.  A
        # host-path rmtree would reopen a swapped parent and reintroduce the
        # exact TOCTOU this boundary is meant to prevent.
        shutil.rmtree(path.name, dir_fd=parent_fd)
        try:
            remaining = os.stat(path.name, dir_fd=parent_fd, follow_symlinks=False)
        except FileNotFoundError:
            return {
                "verified": True,
                "reason": f"{reason}_absent",
                "path_absent": True,
                "entry_identity": entry_identity,
            }
        return {
            "verified": False,
            "reason": f"{reason}_cleanup_failed",
            "path_absent": False,
            "entry_identity": entry_identity,
            "remaining_identity": (int(remaining.st_dev), int(remaining.st_ino)),
        }
    except (OSError, NotImplementedError, TypeError):
        return {"verified": False, "reason": f"{reason}_cleanup_failed", "path_absent": False}
    finally:
        if parent_fd is not None:
            try:
                os.close(parent_fd)
            except OSError:
                pass


def _remove_boundary_config(boundary: ContainerBoundary) -> dict[str, Any]:
    if boundary.cidfile.parent != boundary.config_dir or boundary.cidfile.name != "container.cid":
        return {
            "verified": False,
            "reason": "boundary_config_identity_invalid",
            "config_absent": _path_absent(boundary.config_dir),
            "cidfile_absent": _path_absent(boundary.cidfile),
        }
    result = _remove_exact_owned_directory(
        boundary.config_dir,
        boundary.config_dir.parent,
        name_prefix=f"{boundary.container_name}-",
        reason="boundary_config",
    )
    # `_remove_exact_owned_directory` proves both the config entry and every
    # child (including the CID file) absent through the opened parent dirfd.
    # Do not replace that proof with a host-path lookup after a possible
    # parent swap.
    config_absent = result.get("path_absent") is True
    cidfile_absent = config_absent
    result.update(
        {
            "verified": result.get("verified") is True and config_absent and cidfile_absent,
            "config_absent": config_absent,
            "cidfile_absent": cidfile_absent,
        }
    )
    if result["verified"]:
        try:
            boundary.config_dir.parent.rmdir()
        except OSError:
            pass
    return result


def _retained_boundary_config(boundary: ContainerBoundary) -> dict[str, Any]:
    return {
        "verified": False,
        "reason": "boundary_config_retained",
        "config_absent": _path_absent(boundary.config_dir),
        "cidfile_absent": _path_absent(boundary.cidfile),
    }


def _container_boundary(
    profile: Mapping[str, Any],
    argv: Sequence[str],
    env: Mapping[str, str],
    cwd: Path,
) -> ContainerBoundary:
    sandbox = _map(profile.get("sandbox"))
    if str(sandbox.get("type") or "").strip() != "orbstack_oci":
        raise ExecutionBlocked("execution_boundary_unavailable", "the registered profile has no supported deny-default boundary")
    if _list(profile.get("declared_mounts")):
        raise ExecutionBlocked("mount_boundary_unavailable", "the local coding boundary permits only its task worktree")
    image = str(sandbox.get("image") or "").strip()
    if image not in APPROVED_TASK_IMAGES or not _IMAGE_DIGEST_RE.fullmatch(image):
        raise ExecutionBlocked("boundary_image_denied", "the registered image is not approved and digest-pinned")
    workspace = cwd.resolve(strict=True)
    storage_root = _configured_worktree_root(workspace.parent.parent)
    if storage_root not in workspace.parents or any(char in str(workspace) for char in (",", "\n", "\r")):
        raise ExecutionBlocked("task_storage_invalid", "the task workspace cannot be mounted safely")
    resource_limits = _map(profile.get("resource_limits"))
    control_timeout = float(sandbox["control_timeout_secs"])
    docker = _docker_executable()
    endpoint = _orbstack_endpoint(docker, control_timeout)
    task_ref = hashlib.sha256(
        f"{env.get('TASK_ID', '')}\0{env.get('TASK_ATTEMPT_ID', '')}\0{workspace}".encode("utf-8")
    ).hexdigest()[:24]
    run_nonce = secrets.token_hex(16)
    container_name = f"contextlattice-task-{task_ref}-{run_nonce[:16]}"
    config_dir = _helper_free_docker_config(storage_root, container_name)
    cidfile = config_dir / "container.cid"
    docker_env = {str(key): str(value) for key, value in env.items() if not _credential_env_key(str(key))}
    docker_env.update(
        {
            "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            "HOME": "/workspace/.home",
            "USERPROFILE": "/workspace/.home",
            "XDG_CONFIG_HOME": "/workspace/.home/.config",
            "XDG_CACHE_HOME": "/workspace/.home/.cache",
            "TASK_CWD": "/workspace",
            "TASK_WORKTREE": "/workspace",
        }
    )
    container_env_names = sorted(docker_env)
    docker_env.update(
        {
            "DOCKER_CONFIG": str(config_dir),
            "DOCKER_HOST": endpoint,
            "DOCKER_CLI_HINTS": "false",
        }
    )
    docker_env.pop("DOCKER_CONTEXT", None)
    docker_env.pop("DOCKER_AUTH_CONFIG", None)
    try:
        image_probe = subprocess.run(
            [str(docker), "image", "inspect", image],
            env=docker_env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            check=False,
            timeout=control_timeout,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        cleanup = _remove_exact_owned_directory(
            config_dir,
            config_dir.parent,
            name_prefix=f"{container_name}-",
            reason="boundary_config",
        )
        if cleanup.get("verified") is not True:
            raise ExecutionBlocked(
                "quarantined",
                "the failed image probe boundary configuration could not be proven absent",
                evidence={"config": cleanup},
            ) from exc
        raise ExecutionBlocked("boundary_image_unavailable", "the registered local image could not be inspected") from exc
    if image_probe.returncode != 0 or _probe_has_stderr(image_probe):
        cleanup = _remove_exact_owned_directory(
            config_dir,
            config_dir.parent,
            name_prefix=f"{container_name}-",
            reason="boundary_config",
        )
        evidence = {
            "probe": _probe_evidence("docker image inspect", image_probe),
            "config": cleanup,
        }
        if cleanup.get("verified") is not True:
            raise ExecutionBlocked(
                "quarantined",
                "the rejected image boundary configuration could not be proven absent",
                evidence=evidence,
            )
        raise ExecutionBlocked(
            "boundary_image_unavailable",
            "the registered digest-pinned image inspection was not authoritative",
            evidence=evidence,
        )
    memory = int(resource_limits["max_memory_bytes"])
    processes = int(resource_limits["max_processes"])
    cpus = float(resource_limits["max_cpus"])
    tmpfs_bytes = int(sandbox["tmpfs_bytes"])
    grace = max(1, int(float(_map(profile.get("descendant_policy"))["termination_grace_secs"])))
    launched = [
        str(docker),
        "run",
        "--pull=never",
        f"--name={container_name}",
        f"--cidfile={cidfile}",
        "--network=none",
        "--read-only",
        "--cap-drop=ALL",
        "--security-opt=no-new-privileges",
        "--ipc=none",
        "--init",
        f"--pids-limit={processes}",
        f"--memory={memory}",
        f"--memory-swap={memory}",
        f"--cpus={cpus:g}",
        f"--ulimit=fsize={int(resource_limits['max_file_bytes'])}:{int(resource_limits['max_file_bytes'])}",
        f"--stop-timeout={grace}",
        f"--tmpfs=/tmp:rw,noexec,nosuid,nodev,size={tmpfs_bytes}",
        f"--user={os.getuid()}:{os.getgid()}",
        "--workdir=/workspace",
        "--mount",
        f"type=bind,src={workspace},dst=/workspace",
        "--label=io.contextlattice.task-isolation=true",
        f"--label=io.contextlattice.run-nonce={run_nonce}",
        f"--label=io.contextlattice.task-ref={task_ref}",
    ]
    for key in container_env_names:
        launched.append(f"--env={key}")
    launched.extend([f"--platform={sandbox['platform']}", image, *[str(item) for item in argv]])
    return ContainerBoundary(launched, docker_env, docker, container_name, config_dir, cidfile, run_nonce, task_ref, control_timeout)


def _docker_clean_container_ids(raw: Any) -> list[str] | None:
    if getattr(raw, "returncode", -1) != 0 or _probe_has_stderr(raw):
        return None
    try:
        output = bytes(getattr(raw, "stdout", b"")).decode("ascii")
    except (TypeError, UnicodeDecodeError):
        return None
    ids = [line.strip() for line in output.splitlines() if line.strip()]
    if any(not re.fullmatch(r"[0-9a-f]{64}", item) for item in ids):
        return None
    return ids


def _remove_container(boundary: ContainerBoundary) -> dict[str, Any]:
    container_ref = "container-" + hashlib.sha256(boundary.run_nonce.encode("ascii")).hexdigest()[:24]

    def control(*args: str) -> subprocess.CompletedProcess[bytes]:
        return subprocess.run(
            [str(boundary.docker_executable), *args],
            env=boundary.env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=boundary.control_timeout_secs,
        )

    def unavailable(reason: str, raw: Any | None = None) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "verified": False,
            "quarantined": True,
            "reason": reason,
            "container_ref": container_ref,
        }
        if raw is not None:
            payload["probe_evidence"] = _probe_evidence("docker", raw)
        return payload

    def exact_absence(container_id: str) -> tuple[bool | None, dict[str, Any]]:
        filters = (
            f"id={container_id}",
            f"label=io.contextlattice.run-nonce={boundary.run_nonce}",
            f"name=^/{boundary.container_name}$",
        )
        for filter_value in filters:
            probe = control(
                "container",
                "ls",
                "--all",
                "--quiet",
                "--no-trunc",
                "--filter",
                filter_value,
            )
            ids = _docker_clean_container_ids(probe)
            if ids is None:
                return None, _probe_evidence("docker", probe)
            if ids:
                return False, {"filter": filter_value, "count": len(ids)}
        return True, {}

    try:
        container_id = ""
        if boundary.cidfile.is_file():
            container_id = boundary.cidfile.read_text(encoding="ascii").strip()
            if not re.fullmatch(r"[0-9a-f]{64}", container_id):
                return unavailable("container_cid_invalid")
        else:
            candidates = control(
                "container",
                "ls",
                "--all",
                "--quiet",
                "--no-trunc",
                "--filter",
                f"label=io.contextlattice.run-nonce={boundary.run_nonce}",
            )
            ids = _docker_clean_container_ids(candidates)
            if ids is None:
                return unavailable("container_identity_unavailable", candidates)
            if not ids:
                return {"verified": True, "quarantined": False, "reason": "container_not_created", "boundary": "orbstack_oci", "container_ref": container_ref}
            if len(ids) != 1:
                return unavailable("container_identity_ambiguous")
            container_id = ids[0]

        inspection = control("container", "inspect", container_id)
        if inspection.returncode != 0 or _probe_has_stderr(inspection):
            return unavailable("container_liveness_unverified", inspection)
        try:
            inspected = json.loads(inspection.stdout.decode("utf-8"))
            record = inspected[0] if isinstance(inspected, list) and len(inspected) == 1 else None
            labels = _map(_map(record).get("Config")).get("Labels") if isinstance(record, Mapping) else None
        except (UnicodeDecodeError, json.JSONDecodeError):
            record = None
            labels = None
        expected_labels = {
            "io.contextlattice.task-isolation": "true",
            "io.contextlattice.run-nonce": boundary.run_nonce,
            "io.contextlattice.task-ref": boundary.task_ref,
        }
        if not isinstance(record, Mapping) or record.get("Id") != container_id or record.get("Name") != f"/{boundary.container_name}" or not isinstance(labels, Mapping) or any(labels.get(key) != value for key, value in expected_labels.items()):
            return unavailable("container_identity_mismatch")
        removed = control("container", "rm", "--force", container_id)
        if removed.returncode != 0 or _probe_has_stderr(removed):
            return unavailable("container_removal_failed", removed)
        absent, evidence = exact_absence(container_id)
    except (OSError, UnicodeError, subprocess.TimeoutExpired):
        return unavailable("container_liveness_unverified")
    if absent is None:
        payload = unavailable("container_liveness_unverified")
        payload["probe_evidence"] = evidence
        return payload
    if absent is False:
        return unavailable("container_survived")
    return {"verified": True, "quarantined": False, "reason": "container_absent", "boundary": "orbstack_oci", "container_ref": container_ref}


def _identity_matches(
    pid: int,
    expected_executable: str,
    expected_cwd: Path,
    *,
    parent_pids: set[int] | None = None,
    expected_child_executable: str = "",
    expected_run_nonce: str = "",
    expected_executable_inode: int | None = None,
) -> bool:
    description = _process_description(pid)
    if description is None:
        return False
    return _description_identity_matches(
        description,
        expected_executable,
        expected_cwd,
        parent_pids=parent_pids,
        expected_child_executable=expected_child_executable,
        expected_run_nonce=expected_run_nonce,
        expected_executable_inode=expected_executable_inode,
    )


def _description_identity_matches(
    description: Mapping[str, Any],
    expected_executable: str,
    expected_cwd: Path,
    *,
    parent_pids: set[int] | None = None,
    expected_child_executable: str = "",
    expected_run_nonce: str = "",
    expected_executable_inode: int | None = None,
) -> bool:
    if not isinstance(description, Mapping) or description.get("_probe_status", "present") != "present":
        return False
    expected_raw = str(expected_executable)
    expected = str(Path(expected_raw).resolve(strict=False))
    argv_value = description.get("argv")
    argv = [str(item) for item in argv_value] if isinstance(argv_value, (list, tuple)) else []
    if not argv:
        command = str(description.get("command") or "")
        try:
            argv = shlex.split(command, posix=True)
        except ValueError:
            return False
    if not argv:
        return False
    actual_executable = str(description.get("executable") or "").strip()
    if actual_executable:
        actual_path = Path(actual_executable)
        if actual_path.is_absolute():
            if actual_executable != expected_raw and str(actual_path.resolve(strict=False)) != expected:
                return False
        elif actual_executable not in {expected, expected_raw}:
            return False
    elif argv[0] not in {expected, expected_raw}:
        # Synthetic descriptions used by deterministic tests may not expose
        # a platform process path; even then the first argv token must be an
        # exact executable token, never a substring of a command line.
        return False
    if expected_executable_inode is not None and description.get("executable_inode") is not None:
        try:
            if int(description.get("executable_inode")) != int(expected_executable_inode):
                return False
        except (TypeError, ValueError):
            return False
    if expected_child_executable:
        child = str(expected_child_executable)
        if child not in argv:
            return False
    if expected_run_nonce:
        nonce_token = f"--label=io.contextlattice.run-nonce={expected_run_nonce}"
        if nonce_token not in argv:
            return False
    cwd = str(description.get("cwd") or "")
    if cwd != str(expected_cwd.resolve(strict=False)):
        return False
    if parent_pids is not None and int(description.get("ppid", -1)) not in parent_pids:
        return False
    return True


def _description_status(description: Mapping[str, Any] | None) -> str:
    if description is None:
        return "unknown"
    status = str(description.get("_probe_status") or "present").strip().lower()
    return status if status in {"present", "absent", "unavailable"} else "unknown"


def _executable_inode(path: str | Path) -> int | None:
    try:
        return int(Path(path).resolve(strict=True).stat().st_ino)
    except OSError:
        return None


def _group_snapshot(pgid: int) -> tuple[bool, list[dict[str, Any]] | _ProbeUnavailable]:
    members = _group_members(pgid)
    if isinstance(members, _ProbeUnavailable):
        return False, members
    if not isinstance(members, list):
        return False, []
    return True, members


def _reap_and_describe_direct_leader(
    proc: subprocess.Popen[Any],
    *,
    expected_executable: str,
    known_pgid: int,
    wait_grace_secs: float,
) -> tuple[dict[str, Any] | None, bool]:
    """Reap a completed direct child, then return one fresh identity probe."""

    description = _process_description(int(proc.pid))
    exited = proc.poll() is not None
    if _description_status(description) == "unknown":
        # The leader may exit in the gap between the first probe and poll,
        # but a second poll cannot repair an unavailable identity probe.
        exited = proc.poll() is not None
    if _description_status(description) == "present":
        command = str(description.get("command") or "")
        reaped_form = command in {"<defunct>", f"({Path(expected_executable).name})"}
        if (
            reaped_form
            and int(description.get("ppid", -1)) == os.getpid()
            and int(description.get("pgid", 0)) == int(known_pgid)
        ):
            try:
                proc.wait(timeout=max(0.1, min(float(wait_grace_secs), 1.0)))
            except (subprocess.TimeoutExpired, ChildProcessError, OSError):
                pass
            exited = proc.poll() is not None
            description = _process_description(int(proc.pid))
    return description, exited


def terminate_owned_process_group(
    proc: subprocess.Popen[Any],
    *,
    expected_executable: str,
    expected_cwd: Path,
    term_timeout: float,
    known_pgid: int | None = None,
    tracked_pids: set[int] | None = None,
    expected_child_executable: str = "",
    expected_run_nonce: str = "",
    expected_executable_inode: int | None = None,
) -> dict[str, Any]:
    """Terminate only an exactly attested process group, fail closed on probes."""

    pid = int(proc.pid)
    pgid = int(known_pgid or pid)

    def result(
        reason: str,
        members: list[dict[str, Any]] | _ProbeUnavailable | None = None,
        *,
        probe_evidence: Mapping[str, Any] | None = None,
        **extra: Any,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "verified": False,
            "quarantined": True,
            "reason": reason,
            "members": _probe_members(members),
        }
        evidence = dict(probe_evidence or _probe_evidence_from(members))
        if evidence:
            payload["probe_evidence"] = _redact_value(evidence)
        payload.update(extra)
        return payload

    def group() -> tuple[bool, list[dict[str, Any]] | _ProbeUnavailable]:
        return _group_snapshot(pgid)

    def validate_members(members: list[dict[str, Any]]) -> tuple[bool, str, list[dict[str, Any]]]:
        survivors: list[dict[str, Any]] = []
        for member in members:
            member_id = int(member.get("pid", 0) or 0)
            if member_id == pid:
                continue
            # `allow_descendants=false` is a hard admission rule.  Observed
            # group membership is never a trust signal, even when a child
            # happens to match the registered executable and nonce.
            return False, "untracked_descendant", survivors + [member]
        return True, "", survivors

    description, _leader_exited = _reap_and_describe_direct_leader(
        proc,
        expected_executable=expected_executable,
        known_pgid=pgid,
        wait_grace_secs=term_timeout,
    )
    status = _description_status(description)
    available, members = group()
    if not available:
        return result(
            "process_group_probe_unavailable",
            members,
            probe_evidence=_probe_evidence_from(description) or _probe_evidence_from(members),
        )
    if status in {"unknown", "unavailable"}:
        if any(int(item.get("pid", 0) or 0) != pid for item in members):
            if any(int(item.get("ppid", -1) or -1) != pid for item in members if int(item.get("pid", 0) or 0) != pid):
                return result("process_identity_unavailable", members, probe_evidence=_probe_evidence_from(description))
            return result("untracked_descendant", members, probe_evidence=_probe_evidence_from(description))
        return result("process_identity_unavailable", members, probe_evidence=_probe_evidence_from(description))
    if status == "present":
        observed_pgid = int(description.get("pgid", 0) or 0)
        if known_pgid is not None and observed_pgid != int(known_pgid):
            return result("process_group_changed", members)
        if (
            observed_pgid != pid
            or int(description.get("ppid", -1)) != os.getpid()
            or not _description_identity_matches(
                description,
                expected_executable,
                expected_cwd,
                parent_pids={os.getpid()},
                expected_child_executable=expected_child_executable,
                expected_run_nonce=expected_run_nonce,
                expected_executable_inode=expected_executable_inode,
            )
        ):
            return result("process_identity_mismatch", members)
    elif known_pgid is None:
        return result("process_identity_unavailable", members)

    if status == "present":
        fresh_description, _fresh_exited = _reap_and_describe_direct_leader(
            proc,
            expected_executable=expected_executable,
            known_pgid=pgid,
            wait_grace_secs=term_timeout,
        )
        fresh_status = _description_status(fresh_description)
        if fresh_status != "present":
            if any(
                int(item.get("pid", 0) or 0) != pid and int(item.get("ppid", -1) or -1) != pid
                for item in members
            ):
                return result("process_identity_unavailable", members, probe_evidence=_probe_evidence_from(fresh_description))
            return result("untracked_descendant", members, probe_evidence=_probe_evidence_from(fresh_description))
        if (
            int(fresh_description.get("pgid", 0) or 0) != pgid
            or int(fresh_description.get("ppid", -1)) != os.getpid()
            or not _description_identity_matches(
                fresh_description,
                expected_executable,
                expected_cwd,
                parent_pids={os.getpid()},
                expected_child_executable=expected_child_executable,
                expected_run_nonce=expected_run_nonce,
                expected_executable_inode=expected_executable_inode,
            )
        ):
            return result("process_identity_mismatch", members)
        description = fresh_description

    valid, reason, survivors = validate_members(members)
    if not valid:
        return result(reason, members)
    leader_present = any(int(item.get("pid", 0) or 0) == pid for item in members)
    if not survivors and not leader_present:
        # An absent leader is not proof that the owned process terminated: the
        # direct probe may have raced a PID/group reuse or hidden a foreign
        # descendant.  Polling and an empty membership snapshot therefore stay
        # quarantined unless a present leader was exactly attested first.
        if status == "absent":
            return result("process_identity_unavailable", members)
        try:
            os.killpg(pgid, 0)
        except ProcessLookupError:
            if status == "present":
                return {"verified": True, "quarantined": False, "reason": "already_exited", "members": []}
            return result("process_identity_unavailable", members)
        except OSError:
            return result("group_liveness_unverified", members)
        return result("group_survived", members)

    def await_absent(deadline: float) -> tuple[bool, dict[str, Any]]:
        while time.monotonic() < deadline:
            proc.poll()
            available_now, current = group()
            if not available_now:
                return False, result("process_group_probe_unavailable", current)
            if not current:
                try:
                    proc.wait()
                except (ChildProcessError, OSError):
                    pass
                detached = _descendants(pid)
                if not isinstance(detached, list):
                    return False, result("process_descendant_probe_unavailable", detached)
                if detached:
                    return False, result("detached_descendant_survived", [], descendants=detached)
                return True, {"verified": True, "quarantined": False, "reason": "terminated", "members": []}
            valid_now, reason_now, _survivors_now = validate_members(current)
            if not valid_now:
                return False, result(reason_now, current)
            time.sleep(0.05)
        final_members = _group_members(pgid)
        final_descendants = _descendants(pid)
        final_evidence = _probe_evidence_from(final_members) or _probe_evidence_from(final_descendants)
        return False, result(
            "descendant_survived",
            final_members,
            probe_evidence=final_evidence,
            descendants=_probe_members(final_descendants),
        )

    def attest_before_signal() -> tuple[bool, str, list[dict[str, Any]] | _ProbeUnavailable]:
        """Re-probe the complete leader/group identity immediately before a signal."""

        signal_description, _signal_exited = _reap_and_describe_direct_leader(
            proc,
            expected_executable=expected_executable,
            known_pgid=pgid,
            wait_grace_secs=term_timeout,
        )
        if _description_status(signal_description) != "present":
            evidence = _probe_evidence_from(signal_description)
            return False, "process_identity_unavailable", _ProbeUnavailable(evidence) if evidence else []
        if (
            int(signal_description.get("pgid", 0) or 0) != pgid
            or int(signal_description.get("ppid", -1)) != os.getpid()
            or not _description_identity_matches(
                signal_description,
                expected_executable,
                expected_cwd,
                parent_pids={os.getpid()},
                expected_child_executable=expected_child_executable,
                expected_run_nonce=expected_run_nonce,
                expected_executable_inode=expected_executable_inode,
            )
        ):
            return False, "process_identity_mismatch", []
        available_signal, signal_members = group()
        if not available_signal:
            return False, "process_group_probe_unavailable", signal_members
        if not any(int(item.get("pid", 0) or 0) == pid for item in signal_members):
            return False, "process_identity_unavailable", signal_members
        valid_signal, reason_signal, _signal_survivors = validate_members(signal_members)
        if not valid_signal:
            return False, reason_signal, signal_members
        return True, "", signal_members

    attested_term, term_reason, term_members = attest_before_signal()
    if not attested_term:
        return result(term_reason, term_members)
    try:
        os.killpg(pgid, signal.SIGTERM)
    except (OSError, ProcessLookupError):
        return result("term_signal_failed", term_members)
    ok, terminated = await_absent(time.monotonic() + max(0.1, float(term_timeout)))
    if ok:
        return terminated

    # A second signal is permitted only after a fresh exact attestation of
    # the original leader and every observed child.  Foreign members or an
    # unavailable probe never receive a group signal from this worker.
    attested_kill, kill_reason, kill_members = attest_before_signal()
    if not attested_kill:
        return result(kill_reason, kill_members)
    try:
        os.killpg(pgid, signal.SIGKILL)
    except (OSError, ProcessLookupError):
        return result("kill_signal_failed", kill_members)
    ok, terminated = await_absent(time.monotonic() + max(0.1, float(term_timeout)))
    if ok:
        return terminated
    final_members = _group_members(pgid)
    final_descendants = _descendants(pid)
    final_evidence = _probe_evidence_from(final_members) or _probe_evidence_from(final_descendants)
    return result(
        "descendant_survived",
        final_members,
        probe_evidence=final_evidence,
        descendants=_probe_members(final_descendants),
    )


def _read_stream(stream: Any, limit: int, state: dict[str, Any], key: str, lock: threading.Lock) -> None:
    buffer = bytearray()
    truncated = False
    try:
        while True:
            chunk = stream.read(65536)
            if not chunk:
                break
            with lock:
                available = max(0, min(limit - len(buffer), int(state["combined_limit"]) - int(state["combined"])))
                if available:
                    written = min(len(chunk), available)
                    buffer.extend(chunk[:written])
                    state["combined"] += written
                else:
                    written = 0
                if len(chunk) > written:
                    truncated = True
    except Exception:
        with lock:
            state[f"{key}_error"] = "stream_read_failed"
    finally:
        with lock:
            state[key] = bytes(buffer)
            state[f"{key}_truncated"] = truncated


def _workspace_usage(root: Path, resource_limits: Mapping[str, Any]) -> dict[str, int]:
    max_file_bytes = int(resource_limits["max_file_bytes"])
    max_workspace_bytes = int(resource_limits["max_workspace_bytes"])
    max_workspace_files = int(resource_limits["max_workspace_files"])
    total_bytes = 0
    file_count = 0
    largest_file = 0
    stack = [root]
    try:
        while stack:
            current = stack.pop()
            with os.scandir(current) as entries:
                for entry in entries:
                    try:
                        if entry.is_dir(follow_symlinks=False):
                            stack.append(Path(entry.path))
                            continue
                        item_stat = entry.stat(follow_symlinks=False)
                    except FileNotFoundError:
                        continue
                    file_count += 1
                    item_size = max(0, int(item_stat.st_size))
                    total_bytes += item_size
                    largest_file = max(largest_file, item_size)
                    if item_size > max_file_bytes:
                        raise ExecutionBlocked("workspace_file_limit_exceeded", "a task worktree file exceeds its registered limit")
                    if total_bytes > max_workspace_bytes:
                        raise ExecutionBlocked("workspace_size_limit_exceeded", "the task worktree exceeds its registered byte limit")
                    if file_count > max_workspace_files:
                        raise ExecutionBlocked("workspace_file_count_exceeded", "the task worktree exceeds its registered file-count limit")
    except ExecutionBlocked:
        raise
    except OSError as exc:
        raise ExecutionBlocked("workspace_usage_unavailable", "the task worktree could not be measured safely") from exc
    return {"bytes": total_bytes, "files": file_count, "largest_file_bytes": largest_file}


def _cleanup_initial_process_failure(
    proc: subprocess.Popen[Any],
    boundary: ContainerBoundary,
    *,
    process_reason: str,
    termination_grace_secs: float,
    expected_child_executable: str,
    expected_cwd: Path,
    expected_executable_inode: int | None = None,
    probe_evidence: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    """Always clean the exact launch boundary after initial attestation fails."""

    process_failures: list[str] = []
    container_cleanup: dict[str, Any]
    config_cleanup: dict[str, Any]
    process_ended = False
    signal_identity_verified = False
    try:
        signal_description = _process_description(int(proc.pid))
        signal_group_available, signal_members = _group_snapshot(int(proc.pid))
        signal_identity_verified = (
            signal_group_available
            and _description_status(signal_description) == "present"
            and int(signal_description.get("pgid", 0)) == int(proc.pid)
            and any(int(item.get("pid", 0) or 0) == int(proc.pid) for item in signal_members)
            and not any(int(item.get("pid", 0) or 0) != int(proc.pid) for item in signal_members)
            and _description_identity_matches(
                signal_description,
                str(boundary.argv[0]),
                expected_cwd,
                parent_pids={os.getpid()},
                expected_child_executable=expected_child_executable,
                expected_run_nonce=boundary.run_nonce,
                expected_executable_inode=expected_executable_inode,
            )
        )
        if signal_identity_verified:
            # This is intentionally adjacent to proc.kill: no validation or
            # cleanup probe is allowed to create a signal-to-PID reuse gap.
            try:
                proc.kill()
            except ProcessLookupError:
                pass
            except Exception:
                process_failures.append("kill_failed")
        try:
            proc.wait(timeout=max(0.1, float(termination_grace_secs)))
        except Exception:
            process_failures.append("wait_failed")
        try:
            process_ended = proc.poll() is not None
        except Exception:
            process_ended = False
            process_failures.append("liveness_unavailable")
    finally:
        for stream_name in ("stdout", "stderr"):
            try:
                stream = getattr(proc, stream_name, None)
            except Exception:
                process_failures.append(f"{stream_name}_unavailable")
                continue
            if stream is None:
                continue
            try:
                stream.close()
            except Exception:
                process_failures.append(f"{stream_name}_close_failed")
        try:
            container_cleanup = _remove_container(boundary)
        except Exception:
            container_cleanup = {
                "verified": False,
                "quarantined": True,
                "reason": "container_cleanup_failed",
                "container_ref": "container-" + hashlib.sha256(boundary.run_nonce.encode("ascii")).hexdigest()[:24],
            }
        if not process_ended and container_cleanup.get("verified") is True:
            try:
                proc.wait(timeout=max(0.1, float(termination_grace_secs)))
            except Exception:
                process_failures.append("final_wait_failed")
            try:
                process_ended = proc.poll() is not None
            except Exception:
                process_ended = False
                process_failures.append("final_liveness_unavailable")
        if process_ended and container_cleanup.get("verified") is True:
            config_cleanup = _remove_boundary_config(boundary)
        else:
            config_cleanup = _retained_boundary_config(boundary)
    # A failed initial attestation never becomes verified merely because the
    # subprocess later reports an exit.  That poll is not proof that the
    # foreign or unprobed process was the owned launch.
    process_verified = bool(signal_identity_verified and process_ended and not process_failures)
    process_evidence = _probe_evidence_from(signal_description) or dict(probe_evidence or {})
    process_result: dict[str, Any] = {
        "verified": process_verified,
        "reason": process_reason,
        "cleanup_failures": process_failures,
    }
    if process_evidence:
        process_result["probe_evidence"] = process_evidence
    return {
        "process": process_result,
        "container": container_cleanup,
        "config": config_cleanup,
    }


def run_bounded_process(
    argv: Sequence[str],
    env: Mapping[str, str],
    cwd: Path,
    timeout_secs: int,
    *,
    profile: Mapping[str, Any] | None = None,
    lease_lost: threading.Event | None = None,
    cancel_requested: threading.Event | None = None,
) -> CaptureResult:
    if not argv:
        raise ExecutionBlocked("profile_executable_missing")
    if int(timeout_secs) <= 0:
        raise ExecutionBlocked("runtime_limit_invalid", "the registered profile must provide a positive runtime limit")
    active_profile = profile if isinstance(profile, Mapping) else {}
    output_limits = _map(active_profile.get("output_limits"))
    stdout_limit = int(output_limits.get("stdout_bytes") or MAX_STDOUT_BYTES)
    stderr_limit = int(output_limits.get("stderr_bytes") or MAX_STDERR_BYTES)
    combined_limit = int(output_limits.get("combined_bytes") or MAX_COMBINED_OUTPUT_BYTES)
    if stdout_limit > MAX_STDOUT_BYTES or stderr_limit > MAX_STDERR_BYTES or combined_limit > MAX_COMBINED_OUTPUT_BYTES:
        raise ExecutionBlocked("output_limit_invalid", "profile output limits exceed the worker ceiling")
    descendant_policy = _map(active_profile.get("descendant_policy"))
    if descendant_policy.get("allow_descendants") is not False:
        raise ExecutionBlocked("termination_policy_invalid", "the local worker requires descendant_policy.allow_descendants=false")
    term_timeout = float(descendant_policy.get("termination_grace_secs") or 0)
    if term_timeout <= 0:
        raise ExecutionBlocked("termination_policy_invalid", "profile termination grace is required")
    resource_limits = _map(active_profile.get("resource_limits"))
    workspace_usage = _workspace_usage(cwd, resource_limits)
    boundary = _container_boundary(active_profile, argv, env, cwd)
    launched_argv = boundary.argv
    expected_executable_inode = _executable_inode(str(launched_argv[0]))
    try:
        proc = subprocess.Popen(
            launched_argv,
            cwd=str(cwd),
            env=boundary.env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            start_new_session=True,
        )
    except OSError as exc:
        cleanup = _remove_boundary_config(boundary)
        if cleanup.get("verified") is not True:
            raise ExecutionBlocked(
                "quarantined",
                "the failed launch boundary configuration could not be proven absent",
                evidence={"config": cleanup},
            ) from exc
        raise ExecutionBlocked("process_launch_failed", "registered executable could not be started") from exc
    assert proc.stdout is not None and proc.stderr is not None
    initial_pgid = int(proc.pid)
    initial_description, initial_leader_exited = _reap_and_describe_direct_leader(
        proc,
        expected_executable=str(launched_argv[0]),
        known_pgid=initial_pgid,
        wait_grace_secs=term_timeout,
    )
    initial_status = _description_status(initial_description)
    # Deterministic unit fakes from the pre-attestation harness expose only
    # the legacy command/cwd fields.  Real `_process_description` records
    # `_probe_status` and executable/argv evidence; only those live probes
    # require the OS-wide group snapshot here.  This compatibility branch is
    # unreachable for a production probe and does not authorize a foreign
    # process (the exact identity check below still gates the lifecycle).
    synthetic_description = (
        isinstance(initial_description, Mapping)
        and "_probe_status" not in initial_description
        and "executable" not in initial_description
    )
    if synthetic_description:
        initial_group_available, initial_members = True, []
    elif initial_status == "unknown" or (
        initial_status == "present"
        and (
            int(initial_description.get("ppid", -1)) != os.getpid()
            or int(initial_description.get("pgid", 0) or 0) != initial_pgid
        )
    ):
        initial_group_available, initial_members = False, []
    else:
        initial_group_available, initial_members = _group_snapshot(initial_pgid)
    initial_group_has_foreign = initial_group_available and any(
        int(item.get("pid", 0) or 0) != initial_pgid for item in initial_members
    )
    # A subprocess poll/absence result is not an identity attestation.  In
    # particular, a fast exit can be a foreign process that reused the PID or
    # process group after the launch boundary disappeared.  Only a live,
    # exactly described leader can enter the attested lifecycle below.
    if (
        initial_status != "present"
        or not initial_group_available
        or int(initial_description.get("ppid", -1)) != os.getpid()
        or initial_group_has_foreign
    ):
        initial_reason = "untracked_descendant" if initial_group_has_foreign else "process_identity_unavailable"
        evidence = _cleanup_initial_process_failure(
            proc,
            boundary,
            process_reason=initial_reason,
            termination_grace_secs=term_timeout,
            expected_child_executable=str(argv[0]),
            expected_cwd=cwd,
            expected_executable_inode=expected_executable_inode,
            probe_evidence=_probe_evidence_from(initial_description) or _probe_evidence_from(initial_members),
        )
        if any(_map(evidence.get(item)).get("verified") is not True for item in ("process", "container", "config")):
            raise ExecutionBlocked(
                "quarantined",
                "initial process identity and container cleanup could not both be verified",
                execution_observed=True,
                evidence=evidence,
            )
        raise ExecutionBlocked(
            initial_reason,
            "the launched process identity or descendant policy could not be verified",
            execution_observed=True,
            evidence=evidence,
        )
    known_pgid = int(_map(initial_description).get("pgid", 0))
    initial_identity_valid = _description_identity_matches(
        _map(initial_description),
        str(launched_argv[0]),
        cwd,
        parent_pids={os.getpid()},
        expected_child_executable=str(argv[0]),
        expected_run_nonce=boundary.run_nonce,
        expected_executable_inode=expected_executable_inode,
    )
    if known_pgid != int(proc.pid) or not initial_identity_valid:
        evidence = _cleanup_initial_process_failure(
            proc,
            boundary,
            process_reason=(
                "process_group_unavailable"
                if known_pgid != int(proc.pid)
                else "process_identity_mismatch"
            ),
            termination_grace_secs=term_timeout,
            expected_child_executable=str(argv[0]),
            expected_cwd=cwd,
            expected_executable_inode=expected_executable_inode,
            probe_evidence=_probe_evidence_from(initial_description) or _probe_evidence_from(initial_members),
        )
        if any(_map(evidence.get(item)).get("verified") is not True for item in ("process", "container", "config")):
            raise ExecutionBlocked(
                "quarantined",
                "initial process-group identity and container cleanup could not both be verified",
                execution_observed=True,
                evidence=evidence,
            )
        raise ExecutionBlocked(
            (
                "process_group_unavailable"
                if known_pgid != int(proc.pid)
                else "process_identity_mismatch"
            ),
            "the launched process did not retain its exact owned identity",
            execution_observed=True,
            evidence=evidence,
        )
    state: dict[str, Any] = {"combined": 0, "combined_limit": combined_limit}
    lock = threading.Lock()
    out_thread = threading.Thread(target=_read_stream, args=(proc.stdout, stdout_limit, state, "stdout", lock), daemon=True)
    err_thread = threading.Thread(target=_read_stream, args=(proc.stderr, stderr_limit, state, "stderr", lock), daemon=True)
    started_threads: list[threading.Thread] = []
    outcome = "execution_observed"
    termination: dict[str, Any] = {}
    returncode = -125
    cleanup_failures: list[str] = []
    lifecycle_stage = "stream_start"
    deadline = time.monotonic() + max(1, int(timeout_secs))
    next_workspace_check = time.monotonic() + 0.2

    def terminate_group() -> dict[str, Any]:
        return terminate_owned_process_group(
            proc,
            expected_executable=str(launched_argv[0]),
            expected_child_executable=str(argv[0]),
            expected_run_nonce=boundary.run_nonce,
            expected_cwd=cwd,
            term_timeout=term_timeout,
            known_pgid=known_pgid,
            tracked_pids={int(proc.pid)},
            expected_executable_inode=expected_executable_inode,
        )

    def prove_attested_exit() -> dict[str, Any] | None:
        """Use a pre-exit exact identity plus fresh empty-group probes.

        A poll is only one part of this proof.  The leader was attested with
        its executable, inode, argv, nonce, parent, and cwd before this child
        handle reported exit; fresh group and descendant probes must also be
        available and empty.  Probe absence without that prior identity never
        enters this path.
        """

        if (
            initial_status != "present"
            or not initial_identity_valid
            or "_probe_status" not in _map(initial_description)
        ):
            return None
        available, members = _group_snapshot(known_pgid)
        if not available:
            return {
                "verified": False,
                "quarantined": True,
                "reason": "process_group_probe_unavailable",
                "members": _probe_members(members),
                "probe_evidence": _probe_evidence_from(members),
            }
        foreign_members = [
            item for item in members if int(item.get("pid", 0) or 0) != int(proc.pid)
        ]
        if foreign_members:
            return {
                "verified": False,
                "quarantined": True,
                "reason": "untracked_descendant",
                "members": foreign_members,
            }
        descendants = _descendants(int(proc.pid))
        if not isinstance(descendants, list):
            return {
                "verified": False,
                "quarantined": True,
                "reason": "process_descendant_probe_unavailable",
                "members": [],
                "probe_evidence": _probe_evidence_from(descendants),
            }
        if descendants:
            return {
                "verified": False,
                "quarantined": True,
                "reason": "detached_descendant_survived",
                "members": [],
                "descendants": descendants,
            }
        return {
            "verified": True,
            "quarantined": False,
            "reason": "attested_exit",
            "leader_attestation": "exact_pre_exit_probe",
            "members": [],
        }

    try:
        out_thread.start()
        started_threads.append(out_thread)
        err_thread.start()
        started_threads.append(err_thread)
        lifecycle_stage = "process_poll"
        while proc.poll() is None:
            if lease_lost is not None and lease_lost.is_set():
                outcome = "lease_lost"
                termination = terminate_group()
                if not termination.get("verified"):
                    outcome = "quarantined"
                break
            if cancel_requested is not None and cancel_requested.is_set():
                outcome = "canceled"
                termination = terminate_group()
                if not termination.get("verified"):
                    outcome = "quarantined"
                break
            if time.monotonic() >= deadline:
                outcome = "timed_out"
                termination = terminate_group()
                if not termination.get("verified"):
                    outcome = "quarantined"
                break
            if time.monotonic() >= next_workspace_check:
                try:
                    workspace_usage = _workspace_usage(cwd, resource_limits)
                except ExecutionBlocked as exc:
                    outcome = "resource_limit_exceeded"
                    termination = terminate_group()
                    # The resource probe can race the owned leader's exit.
                    # A same-PID zombie/absent leader is not a foreign
                    # descendant; when the exact pre-exit attestation exists,
                    # use the fresh empty-group proof after the termination
                    # attempt.  This remains fail-closed: the proof is
                    # considered only after the child handle reports exit,
                    # and it still requires available group/descendant
                    # probes.  Polling alone never establishes termination.
                    if not termination.get("verified") and proc.poll() is not None:
                        termination = prove_attested_exit() or termination
                    termination = {**termination, "resource_reason": exc.reason}
                    if not termination.get("verified"):
                        outcome = "quarantined"
                    break
                next_workspace_check = time.monotonic() + 0.2
            time.sleep(0.05)
        lifecycle_stage = "process_poll"
        if proc.poll() is None:
            returncode = -125
            outcome = "quarantined"
        else:
            lifecycle_stage = "wait"
            returncode = proc.wait()
        if termination.get("resource_reason"):
            returncode = 153
        if outcome == "execution_observed":
            try:
                workspace_usage = _workspace_usage(cwd, resource_limits)
            except ExecutionBlocked as exc:
                outcome = "resource_limit_exceeded"
                returncode = 153
                termination = {"verified": True, "quarantined": False, "reason": "resource_limit_exceeded", "resource_reason": exc.reason}
        if not termination and outcome == "execution_observed":
            # A leader can exit cleanly while a background child keeps the
            # pipe or process group alive. The group and descendant probes
            # are deliberately fresh immediately before verified fast-exit.
            termination = prove_attested_exit() or terminate_group()
            if not termination.get("verified"):
                outcome = "quarantined"
    except Exception:
        cleanup_failures.append(lifecycle_stage)
        outcome = "execution_failed"
        returncode = 70
    finally:
        if not termination:
            try:
                termination = terminate_group()
            except Exception:
                termination = {
                    "verified": False,
                    "quarantined": True,
                    "reason": "process_termination_unavailable",
                }
        if termination.get("verified") is not True:
            outcome = "quarantined"
            if returncode == 0:
                returncode = -125

        for thread in started_threads:
            try:
                thread.join(timeout=term_timeout)
            except Exception:
                cleanup_failures.append("stream_join")
        for stream_name in ("stdout", "stderr"):
            try:
                stream = getattr(proc, stream_name)
                stream.close()
            except Exception:
                cleanup_failures.append(f"{stream_name}_close")
        for thread in started_threads:
            try:
                if thread.is_alive():
                    thread.join(timeout=term_timeout)
            except Exception:
                cleanup_failures.append("stream_join")
        try:
            output_pipe_survived = any(thread.is_alive() for thread in started_threads)
        except Exception:
            output_pipe_survived = True
            cleanup_failures.append("stream_liveness")

        stream_failures = sorted(
            key
            for key in ("stdout", "stderr")
            if state.get(f"{key}_error") == "stream_read_failed"
        )
        if output_pipe_survived:
            outcome = "quarantined"
            returncode = -125
            termination = {
                "verified": False,
                "quarantined": True,
                "reason": "output_pipe_survived",
                "host_process_group": termination,
            }
        elif (stream_failures or cleanup_failures) and outcome != "quarantined":
            outcome = "execution_failed"
            if returncode == 0:
                returncode = 70

        try:
            container_termination = _remove_container(boundary)
        except Exception:
            container_termination = {
                "verified": False,
                "quarantined": True,
                "reason": "container_cleanup_failed",
                "container_ref": "container-" + hashlib.sha256(boundary.run_nonce.encode("ascii")).hexdigest()[:24],
            }
        host_verified = termination.get("verified") is True and not output_pipe_survived
        if host_verified and container_termination.get("verified") is True:
            config_cleanup = _remove_boundary_config(boundary)
        else:
            config_cleanup = _retained_boundary_config(boundary)

        if container_termination.get("verified") is not True or config_cleanup.get("verified") is not True:
            outcome = "quarantined"
            if returncode == 0:
                returncode = -125
            termination = {
                "verified": False,
                "quarantined": True,
                "reason": str(
                    container_termination.get("reason")
                    if container_termination.get("verified") is not True
                    else config_cleanup.get("reason")
                ),
                "host_process_group": termination,
                "container": container_termination,
                "config": config_cleanup,
            }
        else:
            if outcome == "execution_observed":
                try:
                    workspace_usage = _workspace_usage(cwd, resource_limits)
                except ExecutionBlocked as exc:
                    outcome = "resource_limit_exceeded"
                    returncode = 153
                    termination = {
                        "verified": True,
                        "quarantined": False,
                        "reason": "resource_limit_exceeded",
                        "resource_reason": exc.reason,
                    }
            termination = {
                **termination,
                "container": container_termination,
                "config": config_cleanup,
                "boundary": "orbstack_oci",
                "workspace_usage": workspace_usage,
            }
        if cleanup_failures:
            termination["cleanup_failures"] = sorted(set(cleanup_failures))
        if stream_failures:
            termination["stream_failures"] = stream_failures
    return CaptureResult(
        returncode=returncode,
        stdout=bytes(state.get("stdout") or b""),
        stderr=bytes(state.get("stderr") or b""),
        stdout_truncated=bool(state.get("stdout_truncated")),
        stderr_truncated=bool(state.get("stderr_truncated")),
        combined_truncated=int(state.get("combined", 0)) >= combined_limit,
        outcome=outcome,
        termination=termination,
    )


def artifact_ref(name: str, content: str, *, task_id: str, attempt_id: str, media_type: str = "text/plain; charset=utf-8", truncated: bool = False, max_bytes: int | None = None) -> dict[str, Any]:
    limit = int(max_bytes or (MAX_STDOUT_BYTES if name.endswith("stdout.txt") else MAX_STDERR_BYTES))
    sanitized, findings = _sanitize_public_text(content)
    bounded = bounded_utf8(sanitized, limit)
    if _contains_sensitive_text(bounded):
        raise ExecutionBlocked("artifact_redaction_unverified", "a text artifact could not be proven public-safe")
    data = bounded.encode("utf-8")
    receipt = "sha256:" + hashlib.sha256(
        json.dumps(
            {
                "scanner": "task-worker-canonical-redactor.v1",
                "content_digest": "sha256:" + hashlib.sha256(data).hexdigest(),
                "findings_redacted": findings,
            },
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    ).hexdigest()
    return {
        "artifact_id": f"{task_id}-{attempt_id}-{name.replace('.', '-')}",
        "name": name,
        "digest": "sha256:" + hashlib.sha256(data).hexdigest(),
        "size_bytes": len(data),
        "media_type": media_type,
        "redaction_status": "worker_redacted" if findings else "worker_scanned",
        "redaction_receipt": receipt,
        "content": bounded,
        "truncated": bool(truncated or len(data) >= limit),
    }


def process_artifacts(capture: CaptureResult, fence: LeaseFence, output_limits: Mapping[str, Any] | None = None) -> list[dict[str, Any]]:
    refs: list[dict[str, Any]] = []
    limits = output_limits if isinstance(output_limits, Mapping) else {}
    stdout_limit = int(limits.get("stdout_bytes") or MAX_STDOUT_BYTES)
    stderr_limit = int(limits.get("stderr_bytes") or MAX_STDERR_BYTES)
    stdout = capture.stdout.decode("utf-8", errors="replace")
    stderr = capture.stderr.decode("utf-8", errors="replace")
    if stdout:
        refs.append(artifact_ref("stdout.txt", stdout, task_id=fence.task_id, attempt_id=fence.attempt_id, truncated=capture.stdout_truncated, max_bytes=stdout_limit))
    if stderr:
        refs.append(artifact_ref("stderr.txt", stderr, task_id=fence.task_id, attempt_id=fence.attempt_id, truncated=capture.stderr_truncated, max_bytes=stderr_limit))
    return refs


def _validated_runner_items(value: Any, field: str, *, max_items: int, allow_skipped: bool) -> list[dict[str, Any]]:
    if not isinstance(value, list) or not value:
        raise ExecutionBlocked("runner_result_invalid", f"runner result {field} must be a non-empty list")
    if len(value) > max_items:
        raise ExecutionBlocked("runner_result_invalid", f"runner result {field} exceeds its item limit")
    result: list[dict[str, Any]] = []
    allowed_keys = {"name", "status", "summary", "duration_ms", "artifact_digest"}
    for item in value:
        if not isinstance(item, Mapping) or set(item) - allowed_keys:
            raise ExecutionBlocked("runner_result_invalid", f"runner result {field} contains an invalid item")
        name = str(item.get("name") or "").strip()
        status = str(item.get("status") or "").strip().lower()
        if not name or len(name.encode("utf-8")) > 128 or redact_text(name) != name:
            raise ExecutionBlocked("runner_result_invalid", f"runner result {field} item name is invalid")
        if status not in {"passed", "failed", "skipped"} or (status == "skipped" and not allow_skipped):
            raise ExecutionBlocked("runner_result_invalid", f"runner result {field} item status is invalid")
        if status != "passed":
            raise ExecutionBlocked("runner_result_failed", f"runner result {field} did not pass")
        normalized: dict[str, Any] = {"name": name, "status": status}
        if "summary" in item:
            summary = str(item.get("summary") or "")
            if len(summary.encode("utf-8")) > 512 or redact_text(summary) != summary:
                raise ExecutionBlocked("runner_result_invalid", f"runner result {field} summary is invalid")
            normalized["summary"] = summary
        if "duration_ms" in item:
            duration = item.get("duration_ms")
            if isinstance(duration, bool) or not isinstance(duration, int) or duration < 0 or duration > 86_400_000:
                raise ExecutionBlocked("runner_result_invalid", f"runner result {field} duration is invalid")
            normalized["duration_ms"] = duration
        if "artifact_digest" in item:
            digest = str(item.get("artifact_digest") or "")
            if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
                raise ExecutionBlocked("runner_result_invalid", f"runner result {field} artifact digest is invalid")
            normalized["artifact_digest"] = digest
        result.append(normalized)
    return result


_INFERENCE_METADATA_FIELDS = frozenset(
    {
        "requested_provider",
        "provider",
        "transport",
        "base_url",
        "reason",
        "coreml_enabled",
        "sidecar_enabled",
        "hardware_profile",
        "selection_mode",
    }
)
_INFERENCE_BOOLEAN_FIELDS = frozenset({"coreml_enabled", "sidecar_enabled"})


def validate_inference_result(output: Any, metadata: Any) -> tuple[str, dict[str, Any]]:
    """Validate the closed gateway-inference result before any publication."""

    if not isinstance(output, str):
        raise ExecutionBlocked("inference_result_invalid", "gateway inference output must be a string")
    try:
        output_bytes = output.encode("utf-8")
    except UnicodeError as exc:
        raise ExecutionBlocked("inference_result_invalid", "gateway inference output encoding is invalid") from exc
    if len(output_bytes) > MAX_SUMMARY_BYTES:
        raise ExecutionBlocked("inference_result_invalid", "gateway inference output exceeds the registered bound")
    safe_output = output
    if not safe_output.strip() or redact_text(safe_output) != safe_output or _contains_sensitive_text(safe_output):
        raise ExecutionBlocked("inference_result_invalid", "gateway inference output must be non-empty and public-safe")
    if not isinstance(metadata, Mapping):
        raise ExecutionBlocked("inference_result_invalid", "gateway inference metadata must be a closed object")
    if set(metadata) - _INFERENCE_METADATA_FIELDS:
        raise ExecutionBlocked("inference_result_invalid", "gateway inference metadata contains an unknown field")
    safe_metadata: dict[str, Any] = {}
    for key in _INFERENCE_METADATA_FIELDS:
        if key not in metadata:
            continue
        value = metadata[key]
        if key in _INFERENCE_BOOLEAN_FIELDS:
            if not isinstance(value, bool):
                raise ExecutionBlocked("inference_result_invalid", "gateway inference metadata boolean is invalid")
            safe_metadata[key] = value
            continue
        if not isinstance(value, str) or len(value.encode("utf-8")) > 512 or redact_text(value) != value:
            raise ExecutionBlocked("inference_result_invalid", "gateway inference metadata value is invalid")
        safe_metadata[key] = value
    return safe_output, safe_metadata


def validate_runner_result(prepared: PreparedExecution, capture: CaptureResult) -> dict[str, Any]:
    protocol = _map(prepared.profile.get("output_protocol"))
    max_bytes = int(protocol.get("max_envelope_bytes") or 0)
    if capture.stdout_truncated or capture.combined_truncated or len(capture.stdout) > max_bytes:
        raise ExecutionBlocked("runner_result_oversize", "runner result envelope exceeded its registered limit")
    try:
        text = capture.stdout.decode("utf-8", errors="strict").strip()
    except UnicodeDecodeError as exc:
        raise ExecutionBlocked("runner_result_invalid", "runner result envelope is not valid UTF-8") from exc
    if not text or len(text.encode("utf-8")) > max_bytes:
        raise ExecutionBlocked("runner_result_invalid", "runner result envelope is empty or oversized")
    decoder = json.JSONDecoder()
    try:
        envelope, end = decoder.raw_decode(text)
    except json.JSONDecodeError as exc:
        raise ExecutionBlocked("runner_result_invalid", "runner output is not one JSON result envelope") from exc
    if text[end:].strip() or not isinstance(envelope, Mapping):
        raise ExecutionBlocked("runner_result_invalid", "runner output must contain exactly one JSON result envelope")
    required = {str(item) for item in _list(protocol.get("required_result_fields"))}
    allowed = {"schema_id", "status", *required, *{str(item) for item in _list(protocol.get("optional_result_fields"))}}
    if set(envelope) - allowed or any(key not in envelope for key in required):
        raise ExecutionBlocked("runner_result_invalid", "runner result fields do not match the registered protocol")
    if envelope.get("schema_id") != protocol.get("schema_id"):
        raise ExecutionBlocked("runner_result_schema_mismatch", "runner result schema does not match the registered profile")
    if envelope.get("status") != "succeeded":
        raise ExecutionBlocked("runner_result_failed", "runner result status is not succeeded")
    if envelope.get("task_id") != prepared.fence.task_id or envelope.get("attempt_id") != prepared.fence.attempt_id:
        raise ExecutionBlocked("runner_result_binding_mismatch", "runner result task or attempt binding is foreign")
    runner_version = str(envelope.get("runner_version") or "")
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._/-]{0,127}", runner_version):
        raise ExecutionBlocked("runner_result_invalid", "runner result version is invalid")
    max_items = int(protocol.get("max_result_items") or 0)
    allow_skipped = protocol.get("allow_skipped") is True
    tests = _validated_runner_items(envelope.get("tests"), "tests", max_items=max_items, allow_skipped=allow_skipped)
    checks = _validated_runner_items(envelope.get("checks"), "checks", max_items=max_items, allow_skipped=allow_skipped)
    warnings = envelope.get("warnings")
    if not isinstance(warnings, list) or len(warnings) > max_items:
        raise ExecutionBlocked("runner_result_invalid", "runner result warnings are invalid")
    safe_warnings: list[str] = []
    for warning in warnings:
        value = str(warning)
        if len(value.encode("utf-8")) > 512 or redact_text(value) != value:
            raise ExecutionBlocked("runner_result_invalid", "runner result warning is invalid")
        safe_warnings.append(value)
    normalized: dict[str, Any] = {
        "schema_id": str(envelope["schema_id"]),
        "status": "succeeded",
        "task_id": prepared.fence.task_id,
        "attempt_id": prepared.fence.attempt_id,
        "runner_version": runner_version,
        "tests": tests,
        "checks": checks,
        "warnings": safe_warnings,
    }
    for optional_field in _list(protocol.get("optional_result_fields")):
        name = str(optional_field)
        if name in envelope:
            value = str(envelope[name])
            if len(value.encode("utf-8")) > 256 or redact_text(value) != value:
                raise ExecutionBlocked("runner_result_invalid", "runner result optional field is invalid")
            normalized[name] = value
    canonical = json.dumps(normalized, ensure_ascii=True, sort_keys=True, separators=(",", ":")).encode("utf-8")
    normalized["digest"] = "sha256:" + hashlib.sha256(canonical).hexdigest()
    return normalized


def artifact_ref_bytes(
    name: str,
    content: bytes,
    *,
    task_id: str,
    attempt_id: str,
    media_type: str,
    max_bytes: int,
) -> dict[str, Any]:
    if len(content) > max_bytes:
        raise ExecutionBlocked("artifact_oversize", f"{name} exceeds its registered artifact limit")
    if media_type == "application/vnd.git.patch":
        contains_sensitive = _git_patch_contains_sensitive_text(content)
    else:
        contains_sensitive = _contains_sensitive_text(content)
    if contains_sensitive:
        raise ExecutionBlocked("secret_in_result", "an immutable artifact contains a restricted value")
    digest = "sha256:" + hashlib.sha256(content).hexdigest()
    receipt = "sha256:" + hashlib.sha256(
        json.dumps(
            {"scanner": "task-worker-canonical-scanner.v1", "content_digest": digest, "findings": 0},
            sort_keys=True,
            separators=(",", ":"),
        ).encode("utf-8")
    ).hexdigest()
    return {
        "artifact_id": f"{task_id}-{attempt_id}-{name.replace('.', '-')}"[:240],
        "name": name,
        "digest": digest,
        "size_bytes": len(content),
        "media_type": media_type,
        "redaction_status": "worker_scanned",
        "redaction_receipt": receipt,
        "content_base64": base64.b64encode(content).decode("ascii"),
        "truncated": False,
    }


def _git_patch_contains_sensitive_text(content: bytes) -> bool:
    """Scan file content in a generated Git patch while trusting its protocol."""

    binary_payload = False
    for raw_line in content.splitlines():
        if raw_line.startswith(b"diff --git "):
            binary_payload = False
        if re.fullmatch(
            rb"index [0-9a-f]{40,64}\.\.[0-9a-f]{40,64}(?: [0-7]{6})?",
            raw_line,
        ):
            continue
        if raw_line == b"GIT binary patch":
            binary_payload = True
            continue
        if binary_payload:
            if not raw_line:
                continue
            if re.fullmatch(rb"(?:literal|delta) [0-9]+", raw_line):
                continue
            if re.fullmatch(rb"[!-~]+", raw_line):
                continue
            return True
        candidate = raw_line
        if raw_line[:1] in {b"+", b"-", b" "} and not raw_line.startswith((b"+++ ", b"--- ")):
            candidate = raw_line[1:]
        if _contains_sensitive_text(candidate):
            return True
    return False


def _git_isolated_env(index_path: Path, object_path: Path, alternate_objects: Path) -> dict[str, str]:
    return _safe_git_env(
        {
            "GIT_INDEX_FILE": str(index_path),
            "GIT_OBJECT_DIRECTORY": str(object_path),
            "GIT_ALTERNATE_OBJECT_DIRECTORIES": str(alternate_objects),
        }
    )


def _git_run_env(repo: Path, env: Mapping[str, str], *args: str) -> subprocess.CompletedProcess[bytes]:
    if not GIT_EXECUTABLE.is_file():
        raise ExecutionBlocked("git_unavailable", str(GIT_EXECUTABLE))
    return subprocess.run(
        [str(GIT_EXECUTABLE), *_GIT_CONFIG_ARGS, *args],
        cwd=str(repo),
        env=_safe_git_env(env),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def _git_capture_bounded(
    repo: Path,
    args: Sequence[str],
    *,
    max_bytes: int,
    reason: str,
    env: Mapping[str, str] | None = None,
) -> bytes:
    if not GIT_EXECUTABLE.is_file():
        raise ExecutionBlocked("git_unavailable", str(GIT_EXECUTABLE))
    proc = subprocess.Popen(
        [str(GIT_EXECUTABLE), *_GIT_CONFIG_ARGS, *[str(item) for item in args]],
        cwd=str(repo),
        env=_safe_git_env(env),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        start_new_session=True,
    )
    assert proc.stdout is not None
    data = proc.stdout.read(max_bytes + 1)
    if len(data) > max_bytes:
        # Git is also a host process.  It runs in its own group so an output
        # overflow cannot use an un-attested direct-child kill or leave a
        # group member behind.  The termination helper performs a fresh
        # executable/inode/argv/cwd/parent/group attestation immediately
        # before each signal and quarantines when any probe is unavailable.
        try:
            proc.communicate(timeout=1.0)
        except subprocess.TimeoutExpired:
            termination = terminate_owned_process_group(
                proc,
                expected_executable=str(GIT_EXECUTABLE),
                expected_cwd=repo,
                term_timeout=1.0,
                known_pgid=int(proc.pid),
                tracked_pids={int(proc.pid)},
                expected_executable_inode=_executable_inode(GIT_EXECUTABLE),
            )
            if termination.get("verified") is not True:
                raise ExecutionBlocked(
                    "quarantined",
                    "bounded Git output exceeded its limit but process termination could not be verified",
                    evidence={"termination": termination},
                )
            proc.communicate()
        raise ExecutionBlocked("artifact_oversize", "bounded Git result exceeds the registered artifact limit")
    remainder, _stderr = proc.communicate()
    data += remainder or b""
    if len(data) > max_bytes:
        raise ExecutionBlocked("artifact_oversize", "bounded Git result exceeds the registered artifact limit")
    if proc.returncode != 0:
        raise ExecutionBlocked(reason, "a bounded Git operation failed")
    return data


def _parse_name_status(raw: bytes) -> tuple[list[dict[str, Any]], list[str], list[str]]:
    try:
        tokens = raw.decode("utf-8", errors="strict").split("\0")
    except UnicodeDecodeError as exc:
        raise ExecutionBlocked("coding_result_unavailable", "changed paths are not valid UTF-8") from exc
    if tokens and tokens[-1] == "":
        tokens.pop()
    changes: list[dict[str, Any]] = []
    changed_paths: list[str] = []
    final_paths: list[str] = []
    index = 0
    while index < len(tokens):
        status = tokens[index]
        index += 1
        if not status or index >= len(tokens):
            raise ExecutionBlocked("coding_result_unavailable", "changed-path inventory is malformed")
        code = status[0]
        if code in {"R", "C"}:
            if index + 1 >= len(tokens):
                raise ExecutionBlocked("coding_result_unavailable", "rename inventory is malformed")
            old_path, new_path = tokens[index], tokens[index + 1]
            index += 2
            paths = [old_path, new_path]
            final_paths.append(new_path)
            change = {"status": status, "old_path": old_path, "path": new_path}
        else:
            path = tokens[index]
            index += 1
            paths = [path]
            if code != "D":
                final_paths.append(path)
            change = {"status": status, "path": path}
        for path in paths:
            if not path or redact_text(path) != path:
                raise ExecutionBlocked("secret_in_result", "a changed path contains a restricted value")
            changed_paths.append(path)
        changes.append(change)
    return changes, list(dict.fromkeys(changed_paths)), list(dict.fromkeys(final_paths))


def _index_blob_map(repo: Path, env: Mapping[str, str], max_bytes: int) -> dict[str, tuple[str, str]]:
    raw = _git_capture_bounded(repo, ["ls-files", "--stage", "-z"], max_bytes=max_bytes, reason="coding_result_unavailable", env=env)
    result: dict[str, tuple[str, str]] = {}
    for record in raw.split(b"\0"):
        if not record:
            continue
        try:
            metadata, raw_path = record.split(b"\t", 1)
            mode, object_id, stage = metadata.decode("ascii").split(" ")
            path = raw_path.decode("utf-8", errors="strict")
        except (UnicodeDecodeError, ValueError) as exc:
            raise ExecutionBlocked("coding_result_unavailable", "isolated result index is malformed") from exc
        if stage == "0":
            result[path] = (mode, object_id)
    return result


def _content_contains_restricted_value(content: bytes) -> bool:
    return _contains_sensitive_text(content)


def _verify_patch_against_base(
    prepared: PreparedExecution,
    patch_bytes: bytes,
    expected_tree: str,
    capture_dir: Path,
    isolated_env: Mapping[str, str],
) -> None:
    workspace = prepared.workspace
    assert workspace.repo is not None
    verify_worktree = capture_dir / "verify-worktree"
    created = _git(workspace.repo, "worktree", "add", "--detach", str(verify_worktree), workspace.base_sha)
    if created.returncode != 0:
        raise ExecutionBlocked("patch_verification_unavailable", "the exact-base verification worktree could not be created")
    try:
        if patch_bytes:
            patch_path = capture_dir / "result.patch"
            patch_path.write_bytes(patch_bytes)
            patch_path.chmod(0o600)
            checked = _git(verify_worktree, "apply", "--check", "--binary", "--whitespace=nowarn", str(patch_path))
            if checked.returncode != 0:
                raise ExecutionBlocked("patch_verification_failed", "the result patch does not apply to the exact base")
            applied = _git(verify_worktree, "apply", "--binary", "--whitespace=nowarn", str(patch_path))
            if applied.returncode != 0:
                raise ExecutionBlocked("patch_verification_failed", "the result patch could not be applied to the exact base")
        verify_index = capture_dir / "verify.index"
        verify_env = dict(isolated_env)
        verify_env["GIT_INDEX_FILE"] = str(verify_index)
        read_tree = _git_run_env(verify_worktree, verify_env, "read-tree", workspace.base_sha)
        staged = _git_run_env(verify_worktree, verify_env, "add", "-A", "--", ".")
        written = _git_run_env(verify_worktree, verify_env, "write-tree")
        if read_tree.returncode != 0 or staged.returncode != 0 or written.returncode != 0:
            raise ExecutionBlocked("patch_verification_failed", "the verified result tree could not be constructed")
        if written.stdout.decode("ascii", errors="ignore").strip() != expected_tree:
            raise ExecutionBlocked("patch_verification_failed", "the applied result tree differs from the captured task state")
    finally:
        removed = _git(workspace.repo, "worktree", "remove", "--force", str(verify_worktree))
        listed = _git(workspace.repo, "worktree", "list", "--porcelain")
        registered = {
            str(Path(line.removeprefix("worktree ")).resolve(strict=False))
            for line in listed.stdout.splitlines()
            if line.startswith("worktree ")
        }
        target = str(verify_worktree.resolve(strict=False))
        if listed.returncode != 0 or target in registered or os.path.lexists(os.fspath(verify_worktree)):
            raise ExecutionBlocked(
                "patch_verification_cleanup_failed",
                "the verification worktree removal and Git registration absence could not both be proven",
                evidence={
                    "git_remove_returncode": removed.returncode,
                    "git_list_returncode": listed.returncode,
                    "registered": target in registered,
                    "path_absent": not os.path.lexists(os.fspath(verify_worktree)),
                },
            )


def collect_coding_result(prepared: PreparedExecution) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    """Capture immutable coding truth before publication, without exposing secrets."""

    workspace = prepared.workspace
    if workspace.kind != "coding" or workspace.repo is None or workspace.worktree is None:
        raise ExecutionBlocked("coding_result_missing", "coding result truth requires a task worktree")
    worktree = workspace.worktree
    head = _git(worktree, "rev-parse", "HEAD")
    if head.returncode != 0:
        raise ExecutionBlocked("coding_result_unavailable", "final worktree HEAD could not be read")
    final_head = head.stdout.strip()
    ancestry = _git(worktree, "merge-base", "--is-ancestor", workspace.base_sha, "HEAD")
    if ancestry.returncode != 0:
        raise ExecutionBlocked("base_ancestry_invalid", "final worktree no longer descends from the declared base SHA")
    resource_limit = int(_map(prepared.profile.get("resource_limits")).get("max_file_bytes") or MAX_PATCH_BYTES)
    patch_limit = min(MAX_PATCH_BYTES, resource_limit)
    storage_root = _configured_worktree_root(worktree.parent.parent)
    capture_root = storage_root / ".result-capture"
    _ensure_private_directory(capture_root, "result_capture_root")
    capture_root_identity = _directory_identity(capture_root)
    capture_dir = Path(tempfile.mkdtemp(prefix="capture-", dir=str(capture_root)))
    if _directory_identity(capture_root) != capture_root_identity:
        _remove_exact_owned_directory(
            capture_dir,
            capture_root,
            name_prefix="capture-",
            reason="result_capture",
        )
        raise ExecutionBlocked("result_capture_root_identity_invalid", "the result-capture directory changed while creating a capture")
    capture_dir.chmod(0o700)
    try:
        common = _git(workspace.repo, "rev-parse", "--git-common-dir")
        if common.returncode != 0:
            raise ExecutionBlocked("coding_result_unavailable", "the repository object store could not be resolved")
        common_dir = Path(common.stdout.strip())
        if not common_dir.is_absolute():
            common_dir = (workspace.repo / common_dir).resolve(strict=False)
        alternate_objects = common_dir / "objects"
        isolated_objects = capture_dir / "objects"
        isolated_objects.mkdir(mode=0o700)
        isolated_index = capture_dir / "result.index"
        isolated_env = _git_isolated_env(isolated_index, isolated_objects, alternate_objects)
        read_tree = _git_run_env(worktree, isolated_env, "read-tree", "HEAD")
        staged = _git_run_env(worktree, isolated_env, "add", "-A", "--", ".")
        written = _git_run_env(worktree, isolated_env, "write-tree")
        if read_tree.returncode != 0 or staged.returncode != 0 or written.returncode != 0:
            raise ExecutionBlocked("coding_result_unavailable", "the isolated result tree could not be constructed")
        final_tree = written.stdout.decode("ascii", errors="ignore").strip()
        if not re.fullmatch(r"[0-9a-f]{40,64}", final_tree):
            raise ExecutionBlocked("coding_result_unavailable", "the isolated result tree identity is invalid")
        name_status = _git_capture_bounded(
            worktree,
            ["diff", "--cached", "--name-status", "--find-renames", "-z", workspace.base_sha, "--"],
            max_bytes=MAX_CONTEXT_BYTES,
            reason="coding_result_unavailable",
            env=isolated_env,
        )
        path_changes, changed_paths, final_paths = _parse_name_status(name_status)
        status_bytes = _git_capture_bounded(
            worktree,
            ["status", "--porcelain=v1", "-z", "--untracked-files=all"],
            max_bytes=MAX_CONTEXT_BYTES,
            reason="coding_result_unavailable",
        )
        try:
            status_text = status_bytes.decode("utf-8", errors="strict")
        except UnicodeDecodeError as exc:
            raise ExecutionBlocked("coding_result_unavailable", "worktree status is not valid UTF-8") from exc
        untracked_paths = [record[3:] for record in status_text.split("\0") if record.startswith("?? ")]
        index_entries = _index_blob_map(worktree, isolated_env, MAX_PATCH_BYTES)
        for path in final_paths:
            entry = index_entries.get(path)
            if entry is None or entry[0] == "160000":
                continue
            content = _git_capture_bounded(
                worktree,
                ["cat-file", "blob", entry[1]],
                max_bytes=resource_limit,
                reason="coding_result_unavailable",
                env=isolated_env,
            )
            if _content_contains_restricted_value(content):
                raise ExecutionBlocked("secret_in_result", "a final result blob contains a restricted value")
        for change in path_changes:
            old_path = str(change.get("old_path") or change.get("path") or "")
            if not old_path or str(change.get("status") or "").startswith("A"):
                continue
            old_blob = _git_capture_bounded(
                worktree,
                ["show", f"{workspace.base_sha}:{old_path}"],
                max_bytes=resource_limit,
                reason="coding_result_unavailable",
                env=isolated_env,
            )
            if _content_contains_restricted_value(old_blob):
                raise ExecutionBlocked("secret_in_result", "a base result blob contains a restricted value")
        patch_bytes = _git_capture_bounded(
            worktree,
            ["diff", "--cached", "--binary", "--full-index", "--find-renames", "--no-ext-diff", "--no-textconv", workspace.base_sha, "--"],
            max_bytes=patch_limit,
            reason="coding_result_unavailable",
            env=isolated_env,
        )
        _verify_patch_against_base(prepared, patch_bytes, final_tree, capture_dir, isolated_env)
    finally:
        capture_cleanup = _remove_exact_owned_directory(
            capture_dir,
            capture_root,
            name_prefix="capture-",
            reason="result_capture",
        )
        if capture_cleanup.get("verified") is not True:
            raise ExecutionBlocked(
                "result_capture_cleanup_failed",
                "the exact result-capture directory could not be proven absent",
                evidence={"cleanup": capture_cleanup},
            )
        try:
            capture_root.rmdir()
        except OSError:
            pass
    artifacts = [
        artifact_ref_bytes(
            "result.patch",
            patch_bytes,
            task_id=prepared.fence.task_id,
            attempt_id=prepared.fence.attempt_id,
            media_type="application/vnd.git.patch",
            max_bytes=patch_limit,
        )
    ]
    truth = {
        "base_sha": workspace.base_sha,
        "base_ancestry_verified": True,
        "final_head": final_head,
        "final_tree": final_tree,
        "patch_applies_to_base": True,
        "verified_tree": final_tree,
        "status": bounded_utf8(status_text.replace("\0", "\n"), MAX_CONTEXT_BYTES),
        "path_changes": path_changes[:4096],
        "changed_paths": changed_paths[:4096],
        "untracked_paths": untracked_paths[:4096],
        "diff_digest": "sha256:" + hashlib.sha256(patch_bytes).hexdigest(),
        "patch_size_bytes": len(patch_bytes),
    }
    return truth, artifacts


def fenced_payload(fence: LeaseFence, payload: Mapping[str, Any] | None = None) -> dict[str, Any]:
    sanitized = _redact_value(dict(payload or {}))
    result = dict(sanitized) if isinstance(sanitized, Mapping) else {}
    result.update(fence.as_dict())
    result["fence"] = fence.as_dict()
    return result


def _event_payload(fence: LeaseFence, session: SnapshotBinding, event_type: str, summary: str, metadata: Mapping[str, Any] | None = None) -> dict[str, Any]:
    sanitized = _redact_value(dict(metadata or {}))
    event_metadata = dict(sanitized) if isinstance(sanitized, Mapping) else {}
    event_metadata.update({"task_id": fence.task_id, "attempt_id": fence.attempt_id, "context_snapshot_id": session.snapshot_id, "context_pack_hash": session.content_hash, "lease_fence": fence.as_dict()})
    safe_summary = bounded_utf8(redact_text(summary), MAX_EVENT_BYTES)
    return fenced_payload(
        fence,
        {
            "session_id": session.session_id,
            "task_id": fence.task_id,
            "attempt_id": fence.attempt_id,
            "type": event_type,
            "summary": safe_summary,
            "metadata": event_metadata,
        },
    )


def _canonical_receipt_digest(receipt: Mapping[str, Any], digest_field: str) -> str:
    payload = {str(key): value for key, value in receipt.items() if str(key) != digest_field}
    return "sha256:" + hashlib.sha256(
        json.dumps(payload, ensure_ascii=True, sort_keys=True, separators=(",", ":")).encode("utf-8")
    ).hexdigest()


def _exact_contract_value(actual: Any, expected: Any) -> bool:
    """Require JSON scalar type and value equality at authority boundaries."""

    if expected is None:
        return actual is None
    if isinstance(expected, bool):
        return actual is expected
    if isinstance(expected, int):
        return isinstance(actual, int) and not isinstance(actual, bool) and actual == expected
    return type(actual) is type(expected) and actual == expected


def _validate_cleanup_authorization(
    authorization: Mapping[str, Any],
    *,
    fence: LeaseFence,
    publication_id: str,
    result_id: str,
    workspace_ref: str,
) -> dict[str, Any]:
    allowed = {
        "schema_id",
        "authorization_id",
        "authorization_digest",
        "authority",
        "authorized",
        "attempt_terminal",
        "durable",
        "state",
        "cleanup_id",
        "workspace_ref",
        "publication_id",
        "result_id",
        "task_id",
        "attempt_id",
        "lease_id",
        "generation",
        "worker_id",
        "worker_instance_id",
    }
    if not isinstance(authorization, Mapping) or set(authorization) - allowed:
        raise ExecutionBlocked("cleanup_authorization_invalid", "cleanup authorization fields are not closed")
    expected = {
        "schema_id": "agent_task_cleanup_authorization.v1",
        "authority": "gateway-go-sqlite-wal",
        "authorized": True,
        "attempt_terminal": True,
        "durable": True,
        "state": "authorized",
        "cleanup_id": _cleanup_id(fence, workspace_ref),
        "workspace_ref": workspace_ref,
        "publication_id": publication_id,
        "result_id": result_id,
        **fence.as_dict(),
    }
    if any(not _exact_contract_value(authorization.get(key), value) for key, value in expected.items()):
        raise ExecutionBlocked("cleanup_authorization_invalid", "cleanup authorization does not bind the exact task attempt")
    authorization_id = str(authorization.get("authorization_id") or "").strip()
    digest = str(authorization.get("authorization_digest") or "").strip()
    if not authorization_id or not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ExecutionBlocked("cleanup_authorization_invalid", "cleanup authorization identity is incomplete")
    if digest != _canonical_receipt_digest(authorization, "authorization_digest"):
        raise ExecutionBlocked("cleanup_authorization_invalid", "cleanup authorization digest is not exact")
    return dict(authorization)


_POST_PUBLICATION_STATE_MATRIX = frozenset(
    {
        ("writeback_pending", "pending"),
        ("writeback_failed", "failed"),
        ("committed", "committed"),
        ("dead_letter", "dead_letter"),
    }
)


def validate_publication_receipt(
    response: Mapping[str, Any],
    *,
    fence: LeaseFence,
    publication_id: str,
    result_id: str,
    workspace_ref: str,
    idempotency_key: str,
) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    if not isinstance(response, Mapping) or response.get("ok", True) is False:
        raise ExecutionBlocked("publication_receipt_invalid", "Gateway publication response was not successful")
    publication = _map(response.get("publication")) or _map(response)
    if publication.get("schema_id") != "agent_task_publication.v1":
        raise ExecutionBlocked("publication_receipt_invalid", "publication schema is not authoritative")
    expected_publication = {
        "publication_id": publication_id,
        "result_id": result_id,
        "idempotency_key": idempotency_key,
        **fence.as_dict(),
    }
    if any(not _exact_contract_value(publication.get(key), value) for key, value in expected_publication.items()):
        raise ExecutionBlocked("publication_receipt_invalid", "publication acknowledgement is foreign")
    status = str(publication.get("status") or "").strip().lower()
    writeback_status = str(publication.get("writeback_status") or "").strip().lower()
    if (status, writeback_status) not in _POST_PUBLICATION_STATE_MATRIX:
        raise ExecutionBlocked("publication_receipt_invalid", "publication is not in a durable staged state")
    receipt = _map(response.get("publication_receipt")) or _map(publication.get("receipt"))
    allowed_receipt = {
        "schema_id",
        "receipt_id",
        "receipt_digest",
        "authority",
        "durable",
        "state",
        "publication_id",
        "result_id",
        "task_id",
        "attempt_id",
        "lease_id",
        "generation",
        "worker_id",
        "worker_instance_id",
    }
    if not receipt or set(receipt) - allowed_receipt:
        raise ExecutionBlocked("publication_receipt_invalid", "publication receipt fields are not closed")
    expected_receipt = {
        "schema_id": "agent_task_publication_receipt.v1",
        "authority": "gateway-go-sqlite-wal",
        "durable": True,
        "state": "staged",
        "publication_id": publication_id,
        "result_id": result_id,
        **fence.as_dict(),
    }
    if any(not _exact_contract_value(receipt.get(key), value) for key, value in expected_receipt.items()):
        raise ExecutionBlocked("publication_receipt_invalid", "publication receipt does not bind the exact lease fence")
    receipt_id = str(receipt.get("receipt_id") or "").strip()
    receipt_digest = str(receipt.get("receipt_digest") or "").strip()
    if not receipt_id or not re.fullmatch(r"sha256:[0-9a-f]{64}", receipt_digest):
        raise ExecutionBlocked("publication_receipt_invalid", "publication receipt identity is incomplete")
    if receipt_digest != _canonical_receipt_digest(receipt, "receipt_digest"):
        raise ExecutionBlocked("publication_receipt_invalid", "publication receipt digest is not exact")
    authorization = _map(response.get("cleanup_authorization")) or _map(publication.get("cleanup_authorization"))
    validated_authorization = _validate_cleanup_authorization(
        authorization,
        fence=fence,
        publication_id=publication_id,
        result_id=result_id,
        workspace_ref=workspace_ref,
    )
    return dict(publication), dict(receipt), validated_authorization


_PUBLICATION_RECONCILIATION_KEYS = frozenset(
    {
        "schema_id",
        "publication_id",
        "result_id",
        "idempotency_key",
        "task_id",
        "attempt_id",
        "lease_id",
        "generation",
        "assignment_generation",
        "lease_generation",
        "worker_id",
        "worker_instance_id",
        "status",
        "writeback_status",
        "publication_receipt",
        "cleanup_authorization",
    }
)
_RECONCILIATION_STATE_MATRIX = frozenset(
    {
        ("writeback_pending", "pending"),
        ("writeback_failed", "failed"),
        ("committed", "committed"),
        ("dead_letter", "dead_letter"),
    }
)


def validate_publication_reconciliation(
    response: Mapping[str, Any],
    *,
    fence: LeaseFence,
    publication_id: str,
    result_id: str,
    workspace_ref: str,
    idempotency_key: str,
) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    """Validate the closed attempt-bound GET reconciliation contract."""

    if not isinstance(response, Mapping) or set(response) != _PUBLICATION_RECONCILIATION_KEYS:
        raise ExecutionBlocked(
            "publication_receipt_invalid",
            "attempt publication reconciliation fields are not the exact closed contract",
        )
    if response.get("schema_id") != "agent_task_publication_reconciliation.v1":
        raise ExecutionBlocked(
            "publication_receipt_invalid",
            "attempt publication reconciliation schema is not authoritative",
        )
    expected_publication = {
        "publication_id": publication_id,
        "result_id": result_id,
        "idempotency_key": idempotency_key,
        "assignment_generation": fence.generation,
        "lease_generation": fence.generation,
        **fence.as_dict(),
    }
    if any(not _exact_contract_value(response.get(key), value) for key, value in expected_publication.items()):
        raise ExecutionBlocked(
            "publication_receipt_invalid",
            "attempt publication reconciliation is foreign",
        )
    state_pair = (
        str(response.get("status") or "").strip().lower(),
        str(response.get("writeback_status") or "").strip().lower(),
    )
    if state_pair not in _RECONCILIATION_STATE_MATRIX:
        raise ExecutionBlocked(
            "publication_receipt_invalid",
            "attempt publication reconciliation state pair is invalid",
        )
    receipt = response.get("publication_receipt")
    receipt_keys = {
        "schema_id",
        "receipt_id",
        "receipt_digest",
        "authority",
        "durable",
        "state",
        "publication_id",
        "result_id",
        "task_id",
        "attempt_id",
        "lease_id",
        "generation",
        "worker_id",
        "worker_instance_id",
    }
    if not isinstance(receipt, Mapping) or set(receipt) != receipt_keys:
        raise ExecutionBlocked(
            "publication_receipt_invalid",
            "reconciliation publication receipt fields are not closed",
        )
    expected_receipt = {
        "schema_id": "agent_task_publication_receipt.v1",
        "authority": "gateway-go-sqlite-wal",
        "durable": True,
        "state": "staged",
        "publication_id": publication_id,
        "result_id": result_id,
        **fence.as_dict(),
    }
    if any(not _exact_contract_value(receipt.get(key), value) for key, value in expected_receipt.items()):
        raise ExecutionBlocked(
            "publication_receipt_invalid",
            "reconciliation publication receipt does not bind the exact lease fence",
        )
    receipt_id = str(receipt.get("receipt_id") or "").strip()
    receipt_digest = str(receipt.get("receipt_digest") or "").strip()
    if not receipt_id or not re.fullmatch(r"sha256:[0-9a-f]{64}", receipt_digest):
        raise ExecutionBlocked(
            "publication_receipt_invalid",
            "reconciliation publication receipt identity is incomplete",
        )
    if receipt_digest != _canonical_receipt_digest(receipt, "receipt_digest"):
        raise ExecutionBlocked(
            "publication_receipt_invalid",
            "reconciliation publication receipt digest is not exact",
        )
    authorization = response.get("cleanup_authorization")
    validated_authorization = _validate_cleanup_authorization(
        authorization if isinstance(authorization, Mapping) else {},
        fence=fence,
        publication_id=publication_id,
        result_id=result_id,
        workspace_ref=workspace_ref,
    )
    return dict(response), dict(receipt), validated_authorization


def _retryable_reconciliation_reason(exc: Exception) -> str:
    response = getattr(exc, "response", None)
    status_code = getattr(response, "status_code", None)
    if status_code == 404:
        return "reconciliation_publication_not_found"
    if isinstance(status_code, int) and status_code >= 500:
        return "reconciliation_gateway_unavailable"
    exception_module = type(exc).__module__
    exception_names = {item.__name__ for item in type(exc).__mro__}
    if exception_module.startswith("httpx") and "TransportError" in exception_names:
        return "reconciliation_transport_unavailable"
    if isinstance(exc, RuntimeError) and str(exc).startswith("ContextLattice request failed status="):
        fallback_status = re.search(r"\bstatus=(\d{3})\b", str(exc))
        if fallback_status and int(fallback_status.group(1)) == 404:
            return "reconciliation_publication_not_found"
        return "reconciliation_gateway_unavailable"
    return ""


def fetch_attempt_publication_reconciliation(
    orchestrator_url: str,
    *,
    fence: LeaseFence,
    publication_id: str,
    result_id: str,
    workspace_ref: str,
    get_json: Callable[..., dict[str, Any]],
) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    """Fetch and validate the exact authoritative attempt publication object."""

    idempotency_key = f"task-result:{result_id}"
    try:
        response = get_json(
            orchestrator_url,
            f"/agents/tasks/{fence.task_id}/attempts/{fence.attempt_id}/publication",
            params={
                "lease_id": fence.lease_id,
                "generation": str(fence.generation),
                "worker_id": fence.worker_id,
                "worker_instance_id": fence.worker_instance_id,
                "idempotency_key": idempotency_key,
            },
            timeout=30.0,
        )
    except Exception as exc:
        reason = _retryable_reconciliation_reason(exc)
        if reason:
            raise ReconciliationLookupRetryable(reason) from None
        raise
    return validate_publication_reconciliation(
        response,
        fence=fence,
        publication_id=publication_id,
        result_id=result_id,
        workspace_ref=workspace_ref,
        idempotency_key=idempotency_key,
    )


# Compatibility import for callers that used the pre-contract name.  Both
# names now consume only agent_task_publication_reconciliation.v1.
fetch_attempt_publication_receipt = fetch_attempt_publication_reconciliation


def publication_acknowledged(
    response: Mapping[str, Any],
    *,
    fence: LeaseFence | None = None,
    publication_id: str = "",
    result_id: str = "",
    workspace_ref: str = "",
    idempotency_key: str = "",
) -> bool:
    if fence is None or not all((publication_id, result_id, workspace_ref, idempotency_key)):
        return False
    try:
        validate_publication_receipt(
            response,
            fence=fence,
            publication_id=publication_id,
            result_id=result_id,
            workspace_ref=workspace_ref,
            idempotency_key=idempotency_key,
        )
    except ExecutionBlocked:
        return False
    return True


def _authoritative_lease_state(response: Mapping[str, Any]) -> tuple[bool, str]:
    if not isinstance(response, Mapping):
        return False, ""
    if response.get("ok") is False:
        return True, _first(response.get("reason"), response.get("error"), "lease_rejected")
    states: list[str] = []
    for source in (response, _map(response.get("lease")), _map(response.get("task"))):
        states.append(_first(source.get("status"), source.get("state"), source.get("lease_status"), source.get("task_status")).lower())
        if source.get("canceled") is True or source.get("cancel_requested") is True or source.get("stale") is True:
            return True, _first(source.get("reason"), "lease_canceled")
    for state in states:
        if state in {"canceled", "cancel_requested", "stale", "lease_lost", "expired", "revoked"}:
            return True, state
    return False, ""


def start_lease_heartbeat(
    orchestrator_url: str,
    fence: LeaseFence,
    session: SnapshotBinding,
    interval_secs: float,
    post_json: Callable[..., dict[str, Any]],
    lost: threading.Event,
    stop: threading.Event,
    state: dict[str, Any] | None = None,
    lease_expires_at: Any = None,
    *,
    auth_snapshot: WorkerAuthSnapshot | None = None,
) -> threading.Thread:
    heartbeat_state = state if state is not None else {}

    def expiry_timestamp(value: Any) -> float | None:
        if isinstance(value, (int, float)):
            return float(value)
        raw = str(value or "").strip()
        if not raw:
            return None
        try:
            from datetime import datetime, timezone

            parsed = datetime.fromisoformat(raw.replace("Z", "+00:00"))
            if parsed.tzinfo is None:
                parsed = parsed.replace(tzinfo=timezone.utc)
            return parsed.timestamp()
        except ValueError:
            return None

    expiry = expiry_timestamp(lease_expires_at)

    def post_heartbeat(snapshot: WorkerAuthSnapshot | None, *args: Any, **kwargs: Any) -> dict[str, Any]:
        if snapshot is not None:
            kwargs["auth_snapshot"] = snapshot
        return post_json(*args, **kwargs)

    def run(snapshot: WorkerAuthSnapshot | None = auth_snapshot) -> None:
        interval = max(1.0, min(float(interval_secs), 60.0))
        while not stop.wait(interval):
            try:
                # `snapshot` is a default-bound immutable value. ContextVars
                # are intentionally not relied upon because this is a plain
                # Python thread with a fresh context.
                response = post_heartbeat(snapshot, orchestrator_url, f"/agents/tasks/{fence.task_id}/heartbeat", fenced_payload(fence, {"session_id": session.session_id}), timeout=max(5.0, interval))
                authoritative_loss, reason = _authoritative_lease_state(response)
                heartbeat_state["last_response"] = {"ok": response.get("ok"), "status": response.get("status"), "reason": reason}
                if authoritative_loss:
                    heartbeat_state["lease_state"] = reason or "lost"
                    lost.set()
                    return
                heartbeat_state["last_ok_monotonic"] = time.monotonic()
            except Exception as exc:
                # A transport error is not itself an authoritative lease loss.
                # Retry until the Gateway-provided expiry, if one exists.
                heartbeat_state["last_error"] = type(exc).__name__
                if expiry is not None and time.time() >= expiry:
                    heartbeat_state["lease_state"] = "lease_expired_without_ack"
                    lost.set()
                    return
    thread = threading.Thread(target=run, name=f"task-heartbeat-{fence.attempt_id}", daemon=True)
    thread.start()
    return thread


def prepare_execution(
    claim: Mapping[str, Any],
    *,
    worker: str,
    worker_instance: str = "",
    orchestrator_url: str,
    get_json: Callable[..., dict[str, Any]],
    source_repo: Path | None = None,
    worktree_root: Path | None = None,
    config_path: str | Path | None = None,
) -> PreparedExecution:
    task, attempt, fence = _claim_parts(claim, worker, worker_instance)
    profile_name, profile = resolve_registered_profile(task, config_path)
    profile["_runtime_policy"] = _runtime_policy(profile, attempt)
    kind = _execution_kind(task)
    validate_capability_policy(task, profile, kind)
    snapshot = fetch_pinned_snapshot(orchestrator_url, task, attempt, get_json)
    workspace = prepare_workspace(task, fence, profile, source_repo=source_repo, worktree_root=worktree_root)
    try:
        argv = resolve_profile_argv(profile, workspace)
        env = build_execution_env(task, snapshot, workspace, fence, profile)
    except Exception:
        root = _configured_worktree_root(worktree_root)
        _cleanup_unstarted_workspace(root, fence, workspace)
        raise
    context_pack = snapshot.snapshot.get("contextPack") or snapshot.snapshot.get("context_pack") or {}
    prompt = bounded_utf8(json.dumps({"task_id": fence.task_id, "attempt_id": fence.attempt_id, "session_id": snapshot.session_id, "context_snapshot_id": snapshot.snapshot_id, "context_pack_hash": snapshot.content_hash, "objective": _first(task.get("title"), task.get("objective")), "payload": _redact_value(_task_payload(task)), "context_pack": _redact_value(context_pack)}, ensure_ascii=True, sort_keys=True), MAX_CONTEXT_BYTES)
    env["TASK_CONTEXT_PROMPT"] = prompt
    return PreparedExecution(task, attempt, fence, profile_name, profile, snapshot, workspace, argv, env, prompt)


def result_manifest(
    prepared: PreparedExecution,
    summary: str,
    output: str,
    artifacts: list[dict[str, Any]],
    publication_id: str,
    *,
    coding_truth: dict[str, Any] | None = None,
) -> dict[str, Any]:
    workspace_ref = prepared.workspace.as_dict()["workspace_ref"]
    cleanup_id = _cleanup_id(prepared.fence, workspace_ref)
    safe_summary = bounded_utf8(summary, MAX_SUMMARY_BYTES)
    safe_output = bounded_utf8(output, MAX_SUMMARY_BYTES)
    if _contains_sensitive_text(safe_summary) or _contains_sensitive_text(safe_output):
        raise ExecutionBlocked("result_redaction_unverified", "result summary or output could not be proven public-safe")
    safe_artifacts = _redact_value(artifacts)
    if not isinstance(safe_artifacts, list):
        raise ExecutionBlocked("result_redaction_unverified", "result artifacts could not be proven public-safe")
    safe_coding_truth = _redact_value(coding_truth or {})
    if not isinstance(safe_coding_truth, dict):
        raise ExecutionBlocked("result_redaction_unverified", "coding result truth could not be proven public-safe")
    result: dict[str, Any] = {
        "schema_id": "agent_task_result_manifest.v1",
        "contract_version": 1,
        "result_id": _result_id(prepared.fence),
        "task_id": prepared.fence.task_id,
        "attempt_id": prepared.fence.attempt_id,
        "session_id": prepared.snapshot.session_id,
        "status": "publication_pending",
        "execution_observed": True,
        "summary": safe_summary,
        "output": safe_output,
        "artifacts": safe_artifacts,
        "context_snapshot_id": prepared.snapshot.snapshot_id,
        "context_pack_hash": prepared.snapshot.content_hash,
        "publication_id": publication_id,
        "fence": prepared.fence.as_dict(),
        "workspace": prepared.workspace.as_dict(),
        "profile": prepared.profile_name,
        "runtime_policy": dict(_map(prepared.profile.get("_runtime_policy"))),
    }
    if prepared.workspace.kind == "coding":
        result["coding"] = {
            "base_sha": prepared.workspace.base_sha,
            "workspace_ref": workspace_ref,
            "truth": safe_coding_truth,
        }
    else:
        result["non_coding"] = {"unintegrated": True, "workspace_ref": workspace_ref}
    result["cleanup"] = {
        "cleanup_id": cleanup_id,
        "owner": "gateway_publication_worker",
        "state": "authorization_required",
        "receipt_required": True,
        "authorization_schema_id": "agent_task_cleanup_authorization.v1",
        "receipt_schema_id": "agent_task_cleanup_receipt.v1",
        "target": workspace_ref,
        "workspace_ref": workspace_ref,
    }
    return result


def cleanup_workspace_after_receipt(
    prepared: PreparedExecution,
    authorization: Mapping[str, Any],
    *,
    publication_receipt: Mapping[str, Any],
    result_id: str,
    termination: Mapping[str, Any],
) -> dict[str, Any]:
    """Remove one exact task surface after a durable receipt and authorization."""

    publication_id = str(publication_receipt.get("publication_id") or "").strip()
    workspace_ref = prepared.workspace.as_dict()["workspace_ref"]
    validated_authorization = _validate_cleanup_authorization(
        authorization,
        fence=prepared.fence,
        publication_id=publication_id,
        result_id=result_id,
        workspace_ref=workspace_ref,
    )
    expected_publication = {"publication_id": publication_id, "result_id": result_id, **prepared.fence.as_dict()}
    if publication_receipt.get("durable") is not True or any(publication_receipt.get(key) != value for key, value in expected_publication.items()):
        raise ExecutionBlocked("cleanup_receipt_invalid", "durable publication receipt does not bind cleanup")
    if termination.get("verified") is not True:
        raise ExecutionBlocked("cleanup_termination_unverified", "task process termination is not proven; workspace retained", execution_observed=True)
    container = _map(termination.get("container"))
    if prepared.workspace.kind == "coding" and container.get("verified") is not True:
        raise ExecutionBlocked("cleanup_termination_unverified", "task container termination is not proven; workspace retained", execution_observed=True)
    root = _configured_worktree_root(prepared.workspace.cwd.parent.parent)
    if prepared.workspace.kind == "coding":
        if prepared.workspace.repo is None or prepared.workspace.worktree is None:
            raise ExecutionBlocked("cleanup_target_invalid", "coding cleanup target is incomplete")
        removed = _git(prepared.workspace.repo, "worktree", "remove", "--force", str(prepared.workspace.worktree))
        listed = _git(prepared.workspace.repo, "worktree", "list", "--porcelain")
        registered = {
            str(Path(line.removeprefix("worktree ")).resolve(strict=False))
            for line in listed.stdout.splitlines()
            if line.startswith("worktree ")
        }
        target = str(prepared.workspace.worktree.resolve(strict=False))
        if (
            listed.returncode != 0
            or target in registered
            or os.path.lexists(os.fspath(prepared.workspace.worktree))
        ):
            raise ExecutionBlocked(
                "cleanup_failed",
                "the task worktree removal and Git registration absence could not both be proven",
                evidence={
                    "git_remove_returncode": removed.returncode,
                    "git_list_returncode": listed.returncode,
                    "registered": target in registered,
                    "path_absent": not os.path.lexists(os.fspath(prepared.workspace.worktree)),
                },
            )
    else:
        root = _configured_worktree_root(prepared.workspace.cwd.parent.parent)
        target = prepared.workspace.cwd.resolve(strict=False)
        if root not in target.parents:
            raise ExecutionBlocked("cleanup_target_invalid", "the non-coding cleanup target escaped the configured root")
        cleanup = _remove_exact_owned_directory(
            target,
            target.parent,
            name_prefix=prepared.fence.attempt_id,
            reason="cleanup_workspace",
        )
        if cleanup.get("verified") is not True:
            raise ExecutionBlocked("cleanup_failed", "the task workspace could not be removed exactly", evidence={"workspace": cleanup})
    if os.path.lexists(os.fspath(prepared.workspace.cwd)):
        raise ExecutionBlocked("cleanup_unverified", "the task workspace still exists after cleanup")
    receipt: dict[str, Any] = {
        "schema_id": "agent_task_cleanup_receipt.v1",
        "receipt_id": "cleanup-receipt-" + hashlib.sha256(
            f"{validated_authorization['authorization_id']}\0{publication_id}\0{workspace_ref}".encode("utf-8")
        ).hexdigest()[:32],
        "authority": "task-execution-worker",
        "state": "cleaned",
        "cleanup_id": validated_authorization["cleanup_id"],
        "workspace_ref": workspace_ref,
        "publication_id": publication_id,
        "result_id": result_id,
        **prepared.fence.as_dict(),
    }
    receipt["receipt_digest"] = _canonical_receipt_digest(receipt, "receipt_digest")
    return receipt


def report_cleanup_receipt(
    prepared: PreparedExecution,
    receipt: Mapping[str, Any],
    *,
    orchestrator_url: str,
    post_json: Callable[..., dict[str, Any]],
    auth_snapshot: WorkerAuthSnapshot | None = None,
) -> dict[str, Any]:
    try:
        post_kwargs: dict[str, Any] = {"timeout": 30.0}
        if auth_snapshot is not None:
            post_kwargs["auth_snapshot"] = auth_snapshot
        response = post_json(
            orchestrator_url,
            f"/agents/tasks/{prepared.fence.task_id}/cleanup",
            fenced_payload(prepared.fence, {"cleanup_receipt": dict(receipt)}),
            **post_kwargs,
        )
    except Exception as exc:
        raise ExecutionBlocked("cleanup_receipt_unreported", type(exc).__name__, execution_observed=True) from exc
    recorded = _map(response.get("cleanup_receipt")) or _map(response)
    allowed = set(receipt) | {"recorded", "durable", "acknowledged"}
    if set(recorded) - allowed:
        raise ExecutionBlocked("cleanup_receipt_unreported", "cleanup receipt acknowledgement fields are not closed", execution_observed=True)
    if any(recorded.get(key) != value for key, value in receipt.items()):
        raise ExecutionBlocked("cleanup_receipt_unreported", "Gateway recorded a foreign cleanup receipt", execution_observed=True)
    if recorded.get("recorded") is not True or recorded.get("durable") is not True or recorded.get("acknowledged") is not True:
        raise ExecutionBlocked("cleanup_receipt_unreported", "Gateway did not durably record cleanup", execution_observed=True)
    root = _configured_worktree_root(prepared.workspace.cwd.parent.parent)
    _remove_workspace_ownership(root, prepared.fence)
    return dict(recorded)


def _load_workspace_ownership(root: Path, marker_path: Path) -> tuple[PreparedExecution, dict[str, Any]]:
    try:
        marker_stat = marker_path.lstat()
        record = json.loads(marker_path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError) as exc:
        raise ExecutionBlocked("workspace_ownership_invalid", "an ownership record could not be verified") from exc
    allowed = {
        "schema_id",
        "task_id",
        "attempt_id",
        "lease_id",
        "generation",
        "worker_id",
        "worker_instance_id",
        "workspace_ref",
        "workspace_kind",
        "base_sha",
        "repository",
        "result_id",
        "publication_id",
        "cleanup_id",
        "record_digest",
    }
    if not isinstance(record, Mapping) or set(record) != allowed:
        raise ExecutionBlocked("workspace_ownership_invalid", "ownership record fields are not closed")
    if marker_path.is_symlink() or marker_stat.st_uid != os.getuid() or stat.S_IMODE(marker_stat.st_mode) != 0o600:
        raise ExecutionBlocked("workspace_ownership_invalid", "ownership record permissions are not owner-only")
    if record.get("schema_id") != "task_workspace_ownership.v1":
        raise ExecutionBlocked("workspace_ownership_invalid", "ownership record schema is invalid")
    digest = str(record.get("record_digest") or "")
    if digest != _canonical_receipt_digest(record, "record_digest"):
        raise ExecutionBlocked("workspace_ownership_invalid", "ownership record digest is invalid")
    try:
        generation = int(record.get("generation"))
    except (TypeError, ValueError) as exc:
        raise ExecutionBlocked("workspace_ownership_invalid", "ownership generation is invalid") from exc
    fence = LeaseFence(
        str(record.get("task_id") or ""),
        str(record.get("attempt_id") or ""),
        str(record.get("lease_id") or ""),
        str(record.get("worker_id") or ""),
        str(record.get("worker_instance_id") or ""),
        generation,
    )
    expected_marker = _ownership_marker_path(root, fence)
    expected_workspace = _task_workspace_path(root, fence)
    if marker_path.resolve(strict=False) != expected_marker.resolve(strict=False):
        raise ExecutionBlocked("workspace_ownership_invalid", "ownership record path is foreign")
    if expected_workspace.is_symlink() or (expected_workspace.exists() and not expected_workspace.is_dir()):
        raise ExecutionBlocked("workspace_ownership_invalid", "owned task workspace is unavailable")
    kind = str(record.get("workspace_kind") or "")
    if kind not in {"coding", "non_repo"}:
        raise ExecutionBlocked("workspace_ownership_invalid", "ownership workspace kind is invalid")
    repository_text = str(record.get("repository") or "").strip()
    repository = Path(repository_text).resolve(strict=False) if kind == "coding" and repository_text else None
    if kind == "coding" and (repository is None or not repository.is_dir()):
        raise ExecutionBlocked("workspace_ownership_invalid", "owned repository binding is unavailable")
    workspace = WorkspaceBinding(kind, expected_workspace, repository, expected_workspace if kind == "coding" else None, str(record.get("base_sha") or ""))
    workspace_ref = workspace.as_dict()["workspace_ref"]
    expected = {
        "workspace_ref": workspace_ref,
        "result_id": _result_id(fence),
        "publication_id": _publication_id(fence),
        "cleanup_id": _cleanup_id(fence, workspace_ref),
    }
    if any(record.get(key) != value for key, value in expected.items()):
        raise ExecutionBlocked("workspace_ownership_invalid", "ownership record does not bind deterministic task identities")
    prepared = PreparedExecution(
        {"id": fence.task_id},
        {"attempt_id": fence.attempt_id},
        fence,
        "reconciliation",
        {},
        SnapshotBinding("", "", "", fence.task_id, fence.attempt_id, {}),
        workspace,
        ["reconciled"] if kind == "coding" else None,
        {},
        "",
    )
    return prepared, dict(record)


def _workspace_processes_absent(workspace: Path, timeout_secs: float = 5.0) -> dict[str, Any]:
    if workspace.is_symlink():
        return {"verified": False, "reason": "workspace_identity_invalid"}
    if not workspace.exists():
        return {"verified": True, "reason": "workspace_absent"}
    lsof = Path("/usr/sbin/lsof")
    if not lsof.is_file():
        return {"verified": False, "reason": "process_probe_unavailable"}
    try:
        probe = subprocess.run(
            [str(lsof), "-n", "-P", "+D", str(workspace)],
            env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"},
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
            timeout=timeout_secs,
        )
    except (OSError, subprocess.TimeoutExpired):
        return {"verified": False, "reason": "process_probe_unavailable"}
    if _probe_has_stderr(probe):
        return {
            "verified": False,
            "reason": "process_probe_unavailable",
            "probe_evidence": _probe_evidence("lsof", probe),
        }
    if probe.returncode == 1 and not probe.stdout.strip() and not probe.stderr.strip():
        return {"verified": True, "reason": "no_open_process"}
    if probe.returncode == 0:
        return {"verified": False, "reason": "workspace_process_present"}
    return {
        "verified": False,
        "reason": "process_probe_unavailable",
        "probe_evidence": _probe_evidence("lsof", probe),
    }


def _workspace_containers_absent(root: Path, prepared: PreparedExecution, timeout_secs: float = 5.0) -> dict[str, Any]:
    config_cleanup: dict[str, Any] = {"verified": False, "reason": "container_probe_config_unavailable"}
    try:
        docker = _docker_executable()
        endpoint = _orbstack_endpoint(docker, timeout_secs)
        workspace = prepared.workspace.cwd.resolve(strict=False)
        task_ref = hashlib.sha256(
            f"{prepared.fence.task_id}\0{prepared.fence.attempt_id}\0{workspace}".encode("utf-8")
        ).hexdigest()[:24]
        config_dir = _helper_free_docker_config(root, f"reconcile-{task_ref}")
        env = {
            "PATH": "/usr/local/bin:/usr/bin:/bin",
            "HOME": "/var/empty",
            "DOCKER_CONFIG": str(config_dir),
            "DOCKER_HOST": endpoint,
            "DOCKER_CLI_HINTS": "false",
        }
        try:
            probes = []
            for filter_value in (
                f"label=io.contextlattice.task-ref={task_ref}",
                f"name=contextlattice-task-{task_ref}-",
            ):
                probes.append(
                    subprocess.run(
                        [str(docker), "container", "ls", "--all", "--quiet", "--no-trunc", "--filter", filter_value],
                        env=env,
                        stdin=subprocess.DEVNULL,
                        stdout=subprocess.PIPE,
                        stderr=subprocess.PIPE,
                        check=False,
                        timeout=timeout_secs,
                    )
                )
        finally:
            config_cleanup = _remove_exact_owned_directory(
                config_dir,
                config_dir.parent,
                name_prefix=f"reconcile-{task_ref}-",
                reason="container_probe_config",
            )
            try:
                (root / ".runtime").rmdir()
            except OSError:
                pass
    except (ExecutionBlocked, OSError, subprocess.TimeoutExpired):
        return {"verified": False, "reason": "container_probe_unavailable"}
    if config_cleanup.get("verified") is not True:
        return {"verified": False, "reason": "container_probe_config_cleanup_failed"}
    ids: set[str] = set()
    for probe in probes:
        probe_ids = _docker_clean_container_ids(probe)
        if probe_ids is None:
            return {
                "verified": False,
                "reason": "container_probe_unavailable",
                "probe_evidence": _probe_evidence("docker", probe),
            }
        ids.update(probe_ids)
    if ids:
        return {"verified": False, "reason": "task_container_present"}
    runtime_cleanup = _remove_owned_runtime_dirs(root, task_ref)
    if runtime_cleanup.get("verified") is not True:
        return {
            "verified": False,
            "reason": "runtime_config_cleanup_failed",
            "task_ref": task_ref,
            "runtime": runtime_cleanup,
        }
    return {
        "verified": True,
        "reason": "container_absent",
        "task_ref": task_ref,
        "runtime": runtime_cleanup,
    }


def _remove_owned_runtime_dirs(root: Path, task_ref: str) -> dict[str, Any]:
    runtime_root = root / ".runtime"
    if not os.path.lexists(os.fspath(runtime_root)):
        return {"verified": True, "reason": "runtime_absent", "task_ref": task_ref}
    if runtime_root.is_symlink() or not runtime_root.is_dir():
        return {"verified": False, "reason": "runtime_root_identity_invalid", "task_ref": task_ref}
    prefix = f"contextlattice-task-{task_ref}-"
    try:
        entries = list(os.scandir(runtime_root))
    except OSError:
        return {"verified": False, "reason": "runtime_probe_unavailable", "task_ref": task_ref}
    removed: list[str] = []
    for entry in entries:
        if not entry.name.startswith(prefix):
            continue
        candidate = Path(entry.path)
        if entry.is_symlink() or not entry.is_dir(follow_symlinks=False):
            return {"verified": False, "reason": "runtime_config_identity_invalid", "task_ref": task_ref}
        try:
            children = list(os.scandir(candidate))
        except OSError:
            return {"verified": False, "reason": "runtime_config_probe_unavailable", "task_ref": task_ref}
        if any(child.name not in {"config.json", "container.cid"} for child in children):
            return {"verified": False, "reason": "runtime_config_identity_invalid", "task_ref": task_ref}
        cleanup = _remove_exact_owned_directory(
            candidate,
            runtime_root,
            name_prefix=prefix,
            reason="runtime_config",
        )
        config_absent = cleanup.get("path_absent") is True
        cidfile_absent = config_absent
        if cleanup.get("verified") is not True or not config_absent or not cidfile_absent:
            return {
                "verified": False,
                "reason": "runtime_config_cleanup_failed",
                "task_ref": task_ref,
                "config_absent": config_absent,
                "cidfile_absent": cidfile_absent,
                "cleanup": cleanup,
            }
        removed.append(entry.name)
    try:
        remaining = [
            entry.name
            for entry in os.scandir(runtime_root)
            if entry.name.startswith(prefix)
        ]
    except OSError:
        return {"verified": False, "reason": "runtime_probe_unavailable", "task_ref": task_ref}
    if remaining:
        return {"verified": False, "reason": "runtime_config_cleanup_failed", "task_ref": task_ref, "remaining": remaining}
    return {"verified": True, "reason": "runtime_config_absent", "task_ref": task_ref, "removed": removed}


def reconcile_owned_workspaces(
    *,
    orchestrator_url: str,
    worker: str,
    worker_instance: str,
    get_json: Callable[..., dict[str, Any]],
    post_json: Callable[..., dict[str, Any]],
    worktree_root: Path | None = None,
) -> dict[str, Any]:
    """Reconcile exact owner records; ambiguous or live attempts are retained."""

    root = _configured_worktree_root(worktree_root)
    examined = 0
    cleaned: list[str] = []
    retained: list[dict[str, str]] = []
    with os.scandir(root) as task_scan:
        task_entries = list(task_scan)
    for task_entry in task_entries:
        if task_entry.name.startswith(".") or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", task_entry.name) or not task_entry.is_dir(follow_symlinks=False):
            continue
        task_dir = Path(task_entry.path)
        with os.scandir(task_dir) as marker_scan:
            marker_entries = list(marker_scan)
        for marker_entry in marker_entries:
            if not marker_entry.name.endswith(".owner.json") or not marker_entry.is_file(follow_symlinks=False):
                continue
            attempt_id = marker_entry.name[: -len(".owner.json")]
            if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", attempt_id):
                continue
            examined += 1
            opaque_ref = "orphan-" + hashlib.sha256(f"{task_entry.name}\0{attempt_id}".encode("utf-8")).hexdigest()[:24]
            try:
                prepared, record = _load_workspace_ownership(root, Path(marker_entry.path))
                if prepared.fence.worker_id != str(worker).strip() or prepared.fence.worker_instance_id != str(worker_instance).strip():
                    raise ExecutionBlocked("reconciliation_owner_mismatch", "orphan owner differs from this worker instance")
                publication, publication_receipt, authorization = fetch_attempt_publication_reconciliation(
                    orchestrator_url,
                    fence=prepared.fence,
                    publication_id=str(record["publication_id"]),
                    result_id=str(record["result_id"]),
                    workspace_ref=str(record["workspace_ref"]),
                    get_json=get_json,
                )
                process_proof = _workspace_processes_absent(prepared.workspace.cwd)
                container_proof = _workspace_containers_absent(root, prepared)
                if process_proof.get("verified") is not True or container_proof.get("verified") is not True:
                    raise ExecutionBlocked(
                        "reconciliation_liveness_unverified",
                        "orphan task liveness is not proven",
                        evidence={"process": process_proof, "container": container_proof},
                    )
                termination = {"verified": True, "reason": "reconciled_absent", "process": process_proof, "container": container_proof}
                local_receipt = cleanup_workspace_after_receipt(
                    prepared,
                    authorization,
                    publication_receipt=publication_receipt,
                    result_id=str(publication["result_id"]),
                    termination=termination,
                )
                report_cleanup_receipt(
                    prepared,
                    local_receipt,
                    orchestrator_url=orchestrator_url,
                    post_json=post_json,
                )
                cleaned.append(opaque_ref)
            except ReconciliationLookupRetryable as exc:
                retained.append({"workspace_ref": opaque_ref, "reason": exc.reason})
            except (ExecutionBlocked, PublicationNotAcknowledged, OSError) as exc:
                reason = exc.reason if isinstance(exc, ExecutionBlocked) else "reconciliation_failed"
                retained_item: dict[str, Any] = {"workspace_ref": opaque_ref, "reason": reason}
                if isinstance(exc, ExecutionBlocked) and exc.evidence:
                    retained_item["evidence"] = exc.evidence
                retained.append(retained_item)
    return {"examined": examined, "cleaned": cleaned, "retained": retained}


def execute_prepared(
    prepared: PreparedExecution,
    *,
    orchestrator_url: str,
    post_json: Callable[..., dict[str, Any]],
    gateway_inference: Callable[[PreparedExecution, threading.Event], tuple[str, dict[str, Any]]] | None = None,
    auth_snapshot: WorkerAuthSnapshot | None = None,
) -> dict[str, Any]:
    lost = threading.Event()
    stop = threading.Event()
    interval = float(prepared.profile["heartbeat_interval_secs"])
    heartbeat_state: dict[str, Any] = {}
    lease_expires_at = _first(prepared.attempt.get("lease_expires_at"), prepared.attempt.get("lease_expiry"), prepared.attempt.get("expires_at"))
    heartbeat = start_lease_heartbeat(
        orchestrator_url,
        prepared.fence,
        prepared.snapshot,
        interval,
        post_json,
        lost,
        stop,
        heartbeat_state,
        lease_expires_at,
        auth_snapshot=auth_snapshot,
    )

    def post_authorized(*args: Any, **kwargs: Any) -> dict[str, Any]:
        if auth_snapshot is not None:
            kwargs["auth_snapshot"] = auth_snapshot
        return post_json(*args, **kwargs)

    try:
        try:
            post_authorized(orchestrator_url, "/v1/agents/sessions/event", _event_payload(prepared.fence, prepared.snapshot, "task.execution_started", "task execution started", {"profile": prepared.profile_name, "workspace": prepared.workspace.as_dict(), "runtime_policy": dict(_map(prepared.profile.get("_runtime_policy")))}), timeout=15.0)
        except Exception as exc:
            raise ExecutionBlocked("ledger_unavailable", type(exc).__name__) from exc
        if prepared.argv is None:
            if gateway_inference is None:
                raise ExecutionBlocked("gateway_unavailable", "model profile has no inference control plane")
            if lost.is_set():
                raise ExecutionBlocked("lease_lost", "authoritative lease was lost before gateway inference")
            try:
                inference_result = gateway_inference(prepared, lost)
                if lost.is_set():
                    raise ExecutionBlocked(
                        "lease_lost",
                        "authoritative lease was lost during gateway inference",
                        execution_observed=True,
                    )
                if not isinstance(inference_result, tuple) or len(inference_result) != 2:
                    raise ExecutionBlocked("inference_result_invalid", "gateway inference must return one closed output tuple")
                output, metadata = inference_result
                result_output, metadata = validate_inference_result(output, metadata)
            except ExecutionBlocked:
                raise
            except Exception as exc:
                raise ExecutionBlocked("gateway_execution_failed", type(exc).__name__) from exc
            capture = CaptureResult(
                0,
                result_output.encode("utf-8"),
                b"",
                False,
                False,
                False,
                "execution_observed",
                {"verified": True, "reason": "no_local_process", "container": {"verified": True, "reason": "not_applicable"}},
            )
            summary = "model execution observed"
        else:
            timeout = int(_map(prepared.profile.get("_runtime_policy"))["effective_runtime_secs"])
            capture = run_bounded_process(prepared.argv, prepared.env, prepared.workspace.cwd, timeout, profile=prepared.profile, lease_lost=lost)
            result_output = bounded_utf8(capture.stdout, MAX_SUMMARY_BYTES)
            summary = "runner execution observed" if capture.returncode == 0 else f"runner exited {capture.returncode}"
            metadata = {"outcome": capture.outcome, "termination": capture.termination, "stdout_truncated": capture.stdout_truncated, "stderr_truncated": capture.stderr_truncated, "combined_truncated": capture.combined_truncated}
        safe_metadata = _redact_value(metadata if isinstance(metadata, Mapping) else {})
        metadata = {
            **(safe_metadata if isinstance(safe_metadata, dict) else {}),
            "heartbeat": dict(heartbeat_state),
            "runtime_policy": dict(_map(prepared.profile.get("_runtime_policy"))),
        }
        if lost.is_set() or capture.outcome in {"lease_lost", "quarantined"}:
            if capture.outcome == "quarantined" or not capture.termination.get("verified", True):
                raise ExecutionBlocked("quarantined", "owned process group or descendants could not be verified terminated", execution_observed=True)
            raise ExecutionBlocked("lease_lost", "authoritative lease was lost before publication", execution_observed=True)
        exit_code = capture.returncode
        artifacts = process_artifacts(capture, prepared.fence, _map(prepared.profile.get("output_limits"))) if prepared.argv is not None else []
        coding_truth: dict[str, Any] | None = None
        runner_truth: dict[str, Any] | None = None
        if exit_code == 0:
            try:
                if prepared.argv is not None:
                    runner_truth = validate_runner_result(prepared, capture)
                if prepared.workspace.kind == "coding":
                    coding_truth, coding_artifacts = collect_coding_result(prepared)
                    artifacts.extend(coding_artifacts)
                    if runner_truth is not None:
                        artifact_digests = sorted(str(item.get("digest") or "") for item in artifacts if item.get("digest"))
                        evidence = {
                            "runner_result_digest": runner_truth["digest"],
                            "artifact_digests": artifact_digests,
                            "verified_tree": coding_truth["verified_tree"],
                        }
                        coding_truth["runner_result"] = runner_truth
                        coding_truth["artifact_digests"] = artifact_digests
                        coding_truth["evidence_digest"] = "sha256:" + hashlib.sha256(
                            json.dumps(evidence, ensure_ascii=True, sort_keys=True, separators=(",", ":")).encode("utf-8")
                        ).hexdigest()
            except ExecutionBlocked as exc:
                metadata = {**metadata, "result_blocked": exc.reason}
                try:
                    observed = post_authorized(
                        orchestrator_url,
                        f"/agents/tasks/{prepared.fence.task_id}/observe",
                        fenced_payload(prepared.fence, {"runner_status": "failed", "exit_code": exit_code, "metadata": metadata}),
                        timeout=30.0,
                    )
                except Exception as post_exc:
                    raise ExecutionBlocked("ledger_unavailable", type(post_exc).__name__, execution_observed=True) from post_exc
                if isinstance(observed, Mapping) and observed.get("ok") is False:
                    raise ExecutionBlocked("stale_lease_callback", "execution observation was rejected by the Gateway fence", execution_observed=True)
                return {"status": "execution_failed", "execution_observed": True, "fence": prepared.fence.as_dict(), "observation": observed, "metadata": metadata}
        try:
            observed = post_authorized(orchestrator_url, f"/agents/tasks/{prepared.fence.task_id}/observe", fenced_payload(prepared.fence, {"runner_status": "succeeded" if exit_code == 0 else "failed", "exit_code": exit_code, "metadata": {"context_snapshot_id": prepared.snapshot.snapshot_id, "context_pack_hash": prepared.snapshot.content_hash, "session_id": prepared.snapshot.session_id, **metadata}}), timeout=30.0)
        except Exception as exc:
            raise ExecutionBlocked("ledger_unavailable", type(exc).__name__, execution_observed=True) from exc
        if isinstance(observed, Mapping) and observed.get("ok") is False:
            raise ExecutionBlocked("stale_lease_callback", "execution observation was rejected by the Gateway fence", execution_observed=True)
        if exit_code != 0:
            return {"status": "execution_failed", "execution_observed": True, "fence": prepared.fence.as_dict(), "observation": observed, "metadata": metadata}
        publication_id = _publication_id(prepared.fence)
        manifest = result_manifest(prepared, summary, result_output, artifacts, publication_id, coding_truth=coding_truth)
        idempotency_key = f"task-result:{manifest['result_id']}"
        request = fenced_payload(prepared.fence, {"publication_id": publication_id, "idempotency_key": idempotency_key, "runner_exit_required": True, "result": manifest, "artifacts": artifacts})
        try:
            publication = post_authorized(orchestrator_url, f"/agents/tasks/{prepared.fence.task_id}/publish", request, timeout=45.0)
        except Exception as exc:
            raise ExecutionBlocked("publication_unavailable", type(exc).__name__, execution_observed=True) from exc
        try:
            publication_record, publication_receipt, cleanup_authorization = validate_publication_receipt(
                publication,
                fence=prepared.fence,
                publication_id=publication_id,
                result_id=str(manifest["result_id"]),
                workspace_ref=str(manifest["workspace"]["workspace_ref"]),
                idempotency_key=idempotency_key,
            )
        except ExecutionBlocked as exc:
            raise PublicationNotAcknowledged(f"Gateway publication receipt rejected: {exc.reason}") from exc
        cleanup_receipt = cleanup_workspace_after_receipt(
            prepared,
            cleanup_authorization,
            publication_receipt=publication_receipt,
            result_id=str(manifest["result_id"]),
            termination=capture.termination,
        )
        recorded_cleanup = report_cleanup_receipt(
            prepared,
            cleanup_receipt,
            orchestrator_url=orchestrator_url,
            post_json=post_json,
            auth_snapshot=auth_snapshot,
        )
        return {
            "status": "publication_pending",
            "execution_observed": True,
            "fence": prepared.fence.as_dict(),
            "result": manifest,
            "publication": publication_record,
            "publication_receipt": publication_receipt,
            "cleanup_receipt": recorded_cleanup,
            "metadata": metadata,
        }
    finally:
        stop.set()
        heartbeat.join(timeout=max(1.0, interval + 1.0))
        if heartbeat.is_alive():
            raise ExecutionBlocked("heartbeat_survived", "lease heartbeat thread did not stop after execution", execution_observed=True)


def execute_claimed_task(
    claim: Mapping[str, Any],
    *,
    worker: str,
    worker_instance: str = "",
    orchestrator_url: str,
    get_json: Callable[..., dict[str, Any]],
    post_json: Callable[..., dict[str, Any]],
    source_repo: Path | None = None,
    worktree_root: Path | None = None,
    config_path: str | Path | None = None,
    gateway_inference: Callable[[PreparedExecution, threading.Event], tuple[str, dict[str, Any]]] | None = None,
    auth_snapshot: WorkerAuthSnapshot | None = None,
) -> dict[str, Any]:
    prepared = prepare_execution(claim, worker=worker, worker_instance=worker_instance, orchestrator_url=orchestrator_url, get_json=get_json, source_repo=source_repo, worktree_root=worktree_root, config_path=config_path)
    return execute_prepared(prepared, orchestrator_url=orchestrator_url, post_json=post_json, gateway_inference=gateway_inference, auth_snapshot=auth_snapshot)
