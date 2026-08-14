#!/usr/bin/env python3
"""Adversarial, deterministic coverage for the U3 local execution surface."""

from __future__ import annotations

import base64
import copy
import hashlib
import io
import json
import os
import re
import signal
import shutil
import subprocess
import tempfile
import threading
import time
import unittest
from dataclasses import replace
from pathlib import Path
from unittest import mock

from scripts.task_agent_execution import (
    CaptureResult,
    ContainerBoundary,
    ExecutionBlocked,
    LeaseFence,
    PreparedExecution,
    PublicationNotAcknowledged,
    ReconciliationLookupRetryable,
    SnapshotBinding,
    WorkspaceBinding,
    bounded_utf8,
    artifact_ref_bytes,
    build_execution_env,
    claim_has_complete_fence,
    cleanup_workspace_after_receipt,
    collect_coding_result,
    execute_claimed_task,
    execute_prepared,
    fetch_attempt_publication_receipt,
    fetch_pinned_snapshot,
    fenced_payload,
    prepare_workspace,
    process_artifacts,
    publication_acknowledged,
    reconcile_owned_workspaces,
    redact_public_value,
    redact_text,
    resolve_registered_profile,
    result_manifest,
    run_bounded_process,
    terminate_owned_process_group,
    validate_inference_result,
    validate_capability_policy,
    validate_publication_receipt,
    validate_publication_reconciliation,
    validate_runner_result,
)
import scripts.task_agent_execution as task_execution


REPO_ROOT = Path(__file__).resolve().parents[2]
FIXTURE_SOURCE = REPO_ROOT / "scripts" / "task_agent_coding_fixture.sh"
LAUNCHER = REPO_ROOT / "scripts" / "launch_task_agent.sh"
CORE_RECONCILIATION_FIXTURE_COMMIT = "2d661cabfae3af63733c25a68425e7bd5fb2dea0"
CORE_RECONCILIATION_FIXTURES = (
    "agent_task_publication_reconciliation.v1.json",
    "agent_task_publication_reconciliation.v1.writeback_failed.json",
    "agent_task_publication_reconciliation.v1.committed.json",
    "agent_task_publication_reconciliation.v1.dead_letter.json",
)


class TaskAgentExecutionU3Tests(unittest.TestCase):
    def setUp(self) -> None:
        configured_tmp = os.getenv("CONTEXTLATTICE_U3_TEST_TMPDIR")
        temp_parent = Path(configured_tmp).resolve() if configured_tmp else Path(tempfile.gettempdir()).resolve()
        temp_parent.mkdir(parents=True, exist_ok=True)
        self.temp_dir = Path(tempfile.mkdtemp(prefix="task-agent-u3-", dir=str(temp_parent)))
        self.worktree_root = self.new_root("task-worktrees")
        self.original_worktree_root = os.environ.get("CONTEXTLATTICE_TASK_WORKTREE_ROOT")
        os.environ["CONTEXTLATTICE_TASK_WORKTREE_ROOT"] = str(self.worktree_root)

    def tearDown(self) -> None:
        if self.original_worktree_root is None:
            os.environ.pop("CONTEXTLATTICE_TASK_WORKTREE_ROOT", None)
        else:
            os.environ["CONTEXTLATTICE_TASK_WORKTREE_ROOT"] = self.original_worktree_root
        shutil.rmtree(self.temp_dir, ignore_errors=True)

    def new_root(self, name: str) -> Path:
        root = self.temp_dir / name
        root.mkdir(mode=0o700)
        root.chmod(0o700)
        return root

    def profile(self, name: str) -> dict[str, object]:
        return resolve_registered_profile({"execution_profile": name})[1]

    def run_git(self, repo: Path, *args: str) -> subprocess.CompletedProcess[str]:
        env = os.environ.copy()
        env.update(
            {
                "GIT_AUTHOR_NAME": "U3 Test",
                "GIT_AUTHOR_EMAIL": "u3@example.invalid",
                "GIT_COMMITTER_NAME": "U3 Test",
                "GIT_COMMITTER_EMAIL": "u3@example.invalid",
            }
        )
        return subprocess.run(["/usr/bin/git", *args], cwd=repo, env=env, text=True, capture_output=True, check=False)

    def git_repo(self) -> tuple[Path, str]:
        repo = self.temp_dir / f"repo-{time.monotonic_ns()}"
        repo.mkdir()
        self.assertEqual(self.run_git(repo, "init", "-q").returncode, 0)
        (repo / "README.md").write_text("base\n", encoding="utf-8")
        fixture = repo / "scripts" / FIXTURE_SOURCE.name
        fixture.parent.mkdir()
        shutil.copy2(FIXTURE_SOURCE, fixture)
        fixture.chmod(0o755)
        self.assertEqual(self.run_git(repo, "add", "README.md", str(fixture.relative_to(repo))).returncode, 0)
        self.assertEqual(self.run_git(repo, "commit", "-qm", "base").returncode, 0)
        base = self.run_git(repo, "rev-parse", "HEAD").stdout.strip()
        self.assertTrue(base)
        return repo, base

    def claim(
        self,
        *,
        profile: str = "local-model",
        kind: str = "non_repo",
        snapshot_id: str = "snapshot-1",
        content_hash: str = "hash-1",
        repo: Path | None = None,
        base_sha: str = "",
    ) -> dict[str, object]:
        task_id = "task-u3"
        attempt_id = "attempt-u3"
        session_id = "session-u3"
        task: dict[str, object] = {
            "id": task_id,
            "execution_kind": kind,
            "execution_profile": profile,
            "context_request": {
                "snapshot_id": snapshot_id,
                "content_hash": content_hash,
                "session_id": session_id,
            },
            "payload": {},
        }
        if repo is not None:
            task.update({"repo": str(repo), "base_sha": base_sha})
        return {
            "task": task,
            "attempt": {
                "attempt_id": attempt_id,
                "session_id": session_id,
            },
            "lease": {
                "task_id": task_id,
                "attempt_id": attempt_id,
                "lease_id": "lease-u3",
                "worker_id": "worker-u3",
                "worker_instance_id": "instance-u3",
                "generation": 3,
            },
        }

    def snapshot_getter(self, claim: dict[str, object]):
        def get_json(_base: str, _path: str, **_kwargs: object) -> dict[str, object]:
            return {
                "ok": True,
                "snapshot": {
                    "snapshotId": claim["task"]["context_request"]["snapshot_id"],  # type: ignore[index]
                    "contentHash": claim["task"]["context_request"]["content_hash"],  # type: ignore[index]
                    "metadata": {
                        "task_id": "task-u3",
                        "attempt_id": "attempt-u3",
                        "session_id": "session-u3",
                    },
                    "contextPack": {"facts": ["bounded"]},
                },
            }

        return get_json

    def prepared_coding(self) -> PreparedExecution:
        repo, base = self.git_repo()
        claim = self.claim(profile="local-coding", kind="coding", repo=repo, base_sha=base)
        from scripts.task_agent_execution import prepare_execution

        return prepare_execution(
            claim,
            worker="worker-u3",
            orchestrator_url="http://gateway.invalid",
            get_json=self.snapshot_getter(claim),
            source_repo=repo,
            worktree_root=self.worktree_root,
        )

    def finished_process_boundary(
        self,
        prepared: PreparedExecution,
        name: str,
        *,
        stdout: object | None = None,
        stderr: object | None = None,
    ) -> tuple[ContainerBoundary, mock.Mock, dict[str, object]]:
        config_dir = self.new_root(f"{name}-config")
        (config_dir / "config.json").write_text('{"auths":{}}\n', encoding="utf-8")
        cidfile = config_dir / "container.cid"
        cidfile.write_text("a" * 64, encoding="ascii")
        run_nonce = "ab" * 16
        boundary = ContainerBoundary(
            [
                "/bin/true",
                "run",
                f"--label=io.contextlattice.run-nonce={run_nonce}",
                "/workspace/fixture",
            ],
            {"PATH": "/usr/bin:/bin"},
            Path("/bin/true"),
            name,
            config_dir,
            cidfile,
            run_nonce,
            f"task-ref-{name}",
            5.0,
        )
        fake_proc = mock.Mock()
        fake_proc.pid = 424230 + len(name)
        fake_proc.stdout = stdout if stdout is not None else io.BytesIO()
        fake_proc.stderr = stderr if stderr is not None else io.BytesIO()
        fake_proc.poll.return_value = 0
        fake_proc.wait.return_value = 0
        description: dict[str, object] = {
            "pid": fake_proc.pid,
            "ppid": os.getpid(),
            "pgid": fake_proc.pid,
            "command": " ".join(boundary.argv),
            "cwd": str(prepared.workspace.cwd),
        }
        return boundary, fake_proc, description

    def runner_envelope(self, prepared: PreparedExecution) -> dict[str, object]:
        return {
            "schema_id": "runner_result.v1",
            "status": "succeeded",
            "task_id": prepared.fence.task_id,
            "attempt_id": prepared.fence.attempt_id,
            "runner_version": "test-runner/1",
            "tests": [{"name": "fixture", "status": "passed"}],
            "checks": [{"name": "boundary", "status": "passed"}],
            "warnings": [],
        }

    def result_capture(self, envelope: object) -> CaptureResult:
        return CaptureResult(
            0,
            json.dumps(envelope, separators=(",", ":")).encode("utf-8"),
            b"",
            False,
            False,
            False,
            "execution_observed",
            {"verified": True},
        )

    def receipt_digest(self, payload: dict[str, object], field: str) -> str:
        canonical = {key: value for key, value in payload.items() if key != field}
        return "sha256:" + hashlib.sha256(
            json.dumps(canonical, ensure_ascii=True, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ).hexdigest()

    def publication_response(
        self,
        prepared: PreparedExecution,
        manifest: dict[str, object],
        *,
        status: str = "writeback_pending",
        writeback_status: str = "pending",
    ) -> dict[str, object]:
        publication_id = str(manifest["publication_id"])
        result_id = str(manifest["result_id"])
        workspace_ref = str(manifest["workspace"]["workspace_ref"])  # type: ignore[index]
        publication = {
            "schema_id": "agent_task_publication.v1",
            "publication_id": publication_id,
            "result_id": result_id,
            "task_id": prepared.fence.task_id,
            "attempt_id": prepared.fence.attempt_id,
            "status": status,
            "writeback_status": writeback_status,
            "idempotency_key": f"task-result:{result_id}",
            **prepared.fence.as_dict(),
        }
        receipt: dict[str, object] = {
            "schema_id": "agent_task_publication_receipt.v1",
            "receipt_id": f"receipt-{publication_id}",
            "authority": "gateway-go-sqlite-wal",
            "durable": True,
            "state": "staged",
            "publication_id": publication_id,
            "result_id": result_id,
            **prepared.fence.as_dict(),
        }
        receipt["receipt_digest"] = self.receipt_digest(receipt, "receipt_digest")
        authorization: dict[str, object] = {
            "schema_id": "agent_task_cleanup_authorization.v1",
            "authorization_id": f"cleanup-authorization-{prepared.fence.attempt_id}",
            "authority": "gateway-go-sqlite-wal",
            "authorized": True,
            "attempt_terminal": True,
            "durable": True,
            "state": "authorized",
            "cleanup_id": manifest["cleanup"]["cleanup_id"],  # type: ignore[index]
            "workspace_ref": workspace_ref,
            "publication_id": publication_id,
            "result_id": result_id,
            **prepared.fence.as_dict(),
        }
        authorization["authorization_digest"] = self.receipt_digest(authorization, "authorization_digest")
        return {
            "ok": True,
            "publication": publication,
            "publication_receipt": receipt,
            "cleanup_authorization": authorization,
        }

    def restart_publication_response(self, response: dict[str, object]) -> dict[str, object]:
        raw_publication = response["publication"]
        self.assertIsInstance(raw_publication, dict)
        publication = dict(copy.deepcopy(raw_publication))
        publication["schema_id"] = "agent_task_publication_reconciliation.v1"
        publication["assignment_generation"] = publication["generation"]
        publication["lease_generation"] = publication["generation"]
        publication["publication_receipt"] = copy.deepcopy(response["publication_receipt"])
        publication["cleanup_authorization"] = copy.deepcopy(response["cleanup_authorization"])
        return publication

    def route_publication_response(
        self,
        prepared: PreparedExecution,
        manifest: dict[str, object],
        *,
        status: str = "writeback_pending",
        writeback_status: str = "pending",
    ) -> dict[str, object]:
        wrapped = self.publication_response(
            prepared,
            manifest,
            status=status,
            writeback_status=writeback_status,
        )
        raw_publication = wrapped["publication"]
        self.assertIsInstance(raw_publication, dict)
        publication = dict(copy.deepcopy(raw_publication))
        publication["publication_receipt"] = copy.deepcopy(wrapped["publication_receipt"])
        publication["cleanup_authorization"] = copy.deepcopy(wrapped["cleanup_authorization"])
        return publication

    def core_reconciliation_fixture(self, name: str) -> dict[str, object]:
        path = f"config/agent_contracts/fixtures/{name}"
        working_path = REPO_ROOT / path
        if working_path.is_file():
            value = json.loads(working_path.read_text(encoding="utf-8"))
            self.assertIsInstance(value, dict)
            return value
        git_env = os.environ.copy()
        for key in (
            "GIT_DIR",
            "GIT_WORK_TREE",
            "GIT_COMMON_DIR",
            "GIT_INDEX_FILE",
            "GIT_OBJECT_DIRECTORY",
            "GIT_ALTERNATE_OBJECT_DIRECTORIES",
            "GIT_CONFIG",
            "GIT_CONFIG_GLOBAL",
            "GIT_CONFIG_SYSTEM",
            "GIT_CONFIG_COUNT",
        ):
            git_env.pop(key, None)
        for key in list(git_env):
            if re.fullmatch(r"GIT_CONFIG_(?:KEY|VALUE)_\d+", key):
                git_env.pop(key, None)
        git_env.update({"GIT_CONFIG_NOSYSTEM": "1", "LANG": "C", "LC_ALL": "C"})
        loaded = subprocess.run(
            ["/usr/bin/git", "show", f"{CORE_RECONCILIATION_FIXTURE_COMMIT}:{path}"],
            cwd=REPO_ROOT,
            env=git_env,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(loaded.returncode, 0, loaded.stderr)
        value = json.loads(loaded.stdout)
        self.assertIsInstance(value, dict)
        return value

    def test_missing_unknown_and_governed_provider_profiles_fail_closed(self) -> None:
        with self.assertRaises(ExecutionBlocked) as missing:
            resolve_registered_profile({})
        self.assertEqual(missing.exception.reason, "missing_profile")
        with self.assertRaises(ExecutionBlocked) as unknown:
            resolve_registered_profile({"execution_profile": "not-registered"})
        self.assertEqual(unknown.exception.reason, "unknown_profile")
        with self.assertRaises(ExecutionBlocked) as governed:
            resolve_registered_profile({"execution_profile": "local-coding-provider"})
        self.assertEqual(governed.exception.reason, "profile_unavailable")

        registry = json.loads((REPO_ROOT / "config" / "agents" / "agent_profiles.json").read_text(encoding="utf-8"))
        registry["execution_profiles"]["local-coding"]["sandbox"]["image"] = "alpine:3.20"
        mutated = self.temp_dir / "mutated-profiles.json"
        mutated.write_text(json.dumps(registry), encoding="utf-8")
        with self.assertRaises(ExecutionBlocked) as mutable_image:
            resolve_registered_profile({"execution_profile": "local-coding"}, config_path=mutated)
        self.assertEqual(mutable_image.exception.reason, "profile_manifest_invalid")

    def test_profile_descendants_must_be_explicitly_disabled(self) -> None:
        registry = json.loads((REPO_ROOT / "config" / "agents" / "agent_profiles.json").read_text(encoding="utf-8"))
        registry["execution_profiles"]["local-model"]["descendant_policy"]["allow_descendants"] = True
        mutated = self.temp_dir / "descendants-profiles.json"
        mutated.write_text(json.dumps(registry), encoding="utf-8")
        with self.assertRaises(ExecutionBlocked) as blocked:
            resolve_registered_profile({"execution_profile": "local-model"}, config_path=mutated)
        self.assertEqual(blocked.exception.reason, "profile_manifest_invalid")

    def test_missing_worker_instance_never_falls_back_to_worker_id(self) -> None:
        claim = self.claim()
        del claim["lease"]["worker_instance_id"]  # type: ignore[index]
        self.assertFalse(claim_has_complete_fence(claim))

    def test_conflicting_fence_copies_and_requested_worker_identity_fail_closed(self) -> None:
        claim = self.claim()
        claim["fence"] = {**claim["lease"], "lease_id": "foreign-lease"}  # type: ignore[arg-type]
        self.assertFalse(claim_has_complete_fence(claim, "worker-u3", "instance-u3"))
        with self.assertRaises(ExecutionBlocked) as conflict:
            from scripts.task_agent_execution import extract_lease_fence

            extract_lease_fence(claim, "worker-u3", "instance-u3")
        self.assertEqual(conflict.exception.reason, "conflicting_lease_fence")

        exact = self.claim()
        self.assertFalse(claim_has_complete_fence(exact, "foreign-worker", "instance-u3"))
        self.assertFalse(claim_has_complete_fence(exact, "worker-u3", "foreign-instance"))

        unsafe = self.claim()
        unsafe["lease"]["worker_instance_id"] = "path:/Users/example/private"  # type: ignore[index]
        with self.assertRaises(ExecutionBlocked) as invalid:
            from scripts.task_agent_execution import extract_lease_fence

            extract_lease_fence(unsafe, "worker-u3")
        self.assertEqual(invalid.exception.reason, "lease_fence_invalid")
        self.assertNotIn("/Users/example", invalid.exception.detail)

        noncanonical = self.claim()
        noncanonical["lease"]["generation"] = "03"  # type: ignore[index]
        self.assertFalse(claim_has_complete_fence(noncanonical, "worker-u3", "instance-u3"))

    def test_snapshot_requires_exact_id_and_all_linkage(self) -> None:
        claim = self.claim()
        getter = self.snapshot_getter(claim)
        original = getter("", "")
        original["snapshot"]["snapshotId"] = "snapshot-1-suffix"  # type: ignore[index]
        with self.assertRaises(ExecutionBlocked) as mismatch:
            fetch_pinned_snapshot("http://gateway.invalid", claim["task"], claim["attempt"], lambda *_args, **_kwargs: original)  # type: ignore[arg-type]
        self.assertEqual(mismatch.exception.reason, "snapshot_mismatch")

        missing_metadata = getter("", "")
        del missing_metadata["snapshot"]["metadata"]["session_id"]  # type: ignore[index]
        with self.assertRaises(ExecutionBlocked) as missing:
            fetch_pinned_snapshot("http://gateway.invalid", claim["task"], claim["attempt"], lambda *_args, **_kwargs: missing_metadata)  # type: ignore[arg-type]
        self.assertEqual(missing.exception.reason, "snapshot_mismatch")

    def test_policy_denies_mount_egress_and_credentials(self) -> None:
        profile = self.profile("local-coding")
        base = {"execution_kind": "coding", "required_capabilities": []}
        local_mount = str(self.temp_dir / "undeclared-host-path")
        with self.assertRaises(ExecutionBlocked) as mount:
            validate_capability_policy({**base, "mounts": [local_mount]}, profile, "coding")
        self.assertEqual(mount.exception.reason, "mount_denied")
        self.assertNotIn(local_mount, mount.exception.detail)
        with self.assertRaises(ExecutionBlocked) as egress:
            validate_capability_policy({**base, "egress": ["internet"]}, profile, "coding")
        self.assertEqual(egress.exception.reason, "egress_denied")
        with self.assertRaises(ExecutionBlocked) as credential:
            validate_capability_policy({**base, "credentials": ["host-keychain"]}, profile, "coding")
        self.assertEqual(credential.exception.reason, "credential_denied")

    def test_server_owned_worktree_root_is_required_and_validated(self) -> None:
        profile = self.profile("local-model")
        fence = LeaseFence("task-root", "attempt-root", "lease", "worker", "instance", 1)
        previous = os.environ.pop("CONTEXTLATTICE_TASK_WORKTREE_ROOT")
        try:
            with self.assertRaises(ExecutionBlocked) as missing:
                prepare_workspace({"execution_kind": "non_repo"}, fence, profile)
            self.assertEqual(missing.exception.reason, "task_storage_unconfigured")
        finally:
            os.environ["CONTEXTLATTICE_TASK_WORKTREE_ROOT"] = previous
        with self.assertRaises(ExecutionBlocked) as relative:
            prepare_workspace({"execution_kind": "non_repo"}, fence, profile, worktree_root=Path("relative-root"))
        self.assertEqual(relative.exception.reason, "task_storage_invalid")
        escaping_fence = LeaseFence("../escape", "attempt", "lease", "worker", "instance", 1)
        with self.assertRaises(ExecutionBlocked) as escaping:
            prepare_workspace({"execution_kind": "non_repo"}, escaping_fence, profile, worktree_root=self.worktree_root)
        self.assertEqual(escaping.exception.reason, "workspace_identity_invalid")
        collision_path = self.worktree_root / fence.task_id / fence.attempt_id
        collision_path.mkdir(parents=True)
        with self.assertRaises(ExecutionBlocked) as collision:
            prepare_workspace({"execution_kind": "non_repo"}, fence, profile, worktree_root=self.worktree_root)
        self.assertEqual(collision.exception.reason, "workspace_collision")
        self.assertNotIn(str(self.worktree_root), collision.exception.detail)

    def test_exact_clean_worktree_from_dirty_source_and_non_repo_sandbox(self) -> None:
        repo, base = self.git_repo()
        (repo / "local-only.txt").write_text("preserve me\n", encoding="utf-8")
        profile = self.profile("local-coding")
        fence = LeaseFence("task-worktree", "attempt-worktree", "lease", "worker", "instance", 1)
        task = {"execution_kind": "coding", "repo": str(repo), "base_sha": base}
        binding = prepare_workspace(task, fence, profile, source_repo=repo, worktree_root=self.worktree_root)
        self.assertEqual(binding.base_sha, base)
        self.assertTrue(binding.source_checkout_dirty)
        self.assertEqual(self.run_git(binding.cwd, "rev-parse", "HEAD").stdout.strip(), base)
        self.assertEqual(self.run_git(binding.cwd, "status", "--porcelain").stdout, "")
        self.assertTrue((repo / "local-only.txt").exists())

        non_repo_root = self.new_root("non-repo")
        non_repo = prepare_workspace({"execution_kind": "non_repo"}, fence, self.profile("local-model"), worktree_root=non_repo_root)
        self.assertEqual(non_repo.kind, "non_repo")
        self.assertFalse((non_repo.cwd / ".git").exists())

    def test_invalid_base_is_rejected_without_touching_source(self) -> None:
        repo, _base = self.git_repo()
        fence = LeaseFence("task-base", "attempt-base", "lease", "worker", "instance", 1)
        with self.assertRaises(ExecutionBlocked) as blocked:
            prepare_workspace({"execution_kind": "coding", "repo": str(repo), "base_sha": "0" * 40}, fence, self.profile("local-coding"), source_repo=repo, worktree_root=self.worktree_root)
        self.assertEqual(blocked.exception.reason, "base_sha_unavailable")

    def test_branch_tag_abbreviation_and_noncanonical_case_are_not_exact_base_shas(self) -> None:
        repo, base = self.git_repo()
        self.assertEqual(self.run_git(repo, "branch", "mutable-base", base).returncode, 0)
        self.assertEqual(self.run_git(repo, "tag", "mutable-tag", base).returncode, 0)
        self.assertEqual(self.run_git(repo, "tag", "-a", "annotated-tag", "-m", "annotated", base).returncode, 0)
        tag_object = self.run_git(repo, "rev-parse", "annotated-tag").stdout.strip()
        self.assertRegex(tag_object, r"^[0-9a-f]{40}$")
        self.assertNotEqual(tag_object, base)
        fence = LeaseFence("task-ref-base", "attempt-ref-base", "lease", "worker", "instance", 1)
        candidates = (
            ("mutable-base", "base_sha_invalid"),
            ("mutable-tag", "base_sha_invalid"),
            (base[:12], "base_sha_invalid"),
            (base.upper(), "base_sha_invalid"),
            (tag_object, "base_sha_mismatch"),
        )
        for candidate, reason in candidates:
            with self.subTest(candidate=candidate, reason=reason):
                with self.assertRaises(ExecutionBlocked) as blocked:
                    prepare_workspace(
                        {"execution_kind": "coding", "repo": str(repo), "base_sha": candidate},
                        fence,
                        self.profile("local-coding"),
                        source_repo=repo,
                        worktree_root=self.worktree_root,
                    )
                self.assertEqual(blocked.exception.reason, reason)
        self.assertEqual(self.run_git(repo, "rev-parse", "HEAD").stdout.strip(), base)

    def test_ambient_git_selection_and_config_injection_cannot_redirect_exact_base_or_result(self) -> None:
        repo, base = self.git_repo()
        foreign, _foreign_base = self.git_repo()
        fence = LeaseFence("task-git-env", "attempt-git-env", "lease", "worker", "instance", 1)
        poisoned = {
            "GIT_DIR": str(foreign / ".git"),
            "GIT_WORK_TREE": str(foreign),
            "GIT_COMMON_DIR": str(foreign / ".git"),
            "GIT_INDEX_FILE": str(foreign / ".git" / "index"),
            "GIT_OBJECT_DIRECTORY": str(foreign / ".git" / "objects"),
            "GIT_ALTERNATE_OBJECT_DIRECTORIES": str(foreign / ".git" / "objects"),
            "GIT_CONFIG_COUNT": "1",
            "GIT_CONFIG_KEY_0": "core.worktree",
            "GIT_CONFIG_VALUE_0": str(foreign),
        }
        with mock.patch.dict(os.environ, poisoned, clear=False):
            workspace = prepare_workspace(
                {"execution_kind": "coding", "repo": str(repo), "base_sha": base},
                fence,
                self.profile("local-coding"),
                source_repo=repo,
                worktree_root=self.worktree_root,
            )
            (workspace.cwd / "README.md").write_text("target-result\n", encoding="utf-8")
            prepared = PreparedExecution(
                {"id": fence.task_id},
                {"attempt_id": fence.attempt_id},
                fence,
                "local-coding",
                self.profile("local-coding"),
                SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
                workspace,
                None,
                {},
                "",
            )
            truth, artifacts = collect_coding_result(prepared)
        self.assertEqual(self.run_git(workspace.cwd, "rev-parse", "HEAD").stdout.strip(), base)
        self.assertEqual(truth["base_sha"], base)
        self.assertIn(b"target-result", base64.b64decode(artifacts[0]["content_base64"]))
        self.assertNotIn(b"target-result", self.run_git(foreign, "show", "HEAD:README.md").stdout.encode())

    def test_registered_fixture_digest_mismatch_removes_unstarted_worktree(self) -> None:
        repo, _base = self.git_repo()
        fixture = repo / "scripts" / FIXTURE_SOURCE.name
        fixture.write_text(fixture.read_text(encoding="utf-8") + "\n# tampered\n", encoding="utf-8")
        self.assertEqual(self.run_git(repo, "add", str(fixture.relative_to(repo))).returncode, 0)
        self.assertEqual(self.run_git(repo, "commit", "-qm", "tamper fixture").returncode, 0)
        base = self.run_git(repo, "rev-parse", "HEAD").stdout.strip()
        claim = self.claim(profile="local-coding", kind="coding", repo=repo, base_sha=base)

        from scripts.task_agent_execution import prepare_execution

        with self.assertRaises(ExecutionBlocked) as blocked:
            prepare_execution(
                claim,
                worker="worker-u3",
                orchestrator_url="http://gateway.invalid",
                get_json=self.snapshot_getter(claim),
                source_repo=repo,
                worktree_root=self.worktree_root,
            )
        self.assertEqual(blocked.exception.reason, "profile_executable_mismatch")
        rejected_path = self.worktree_root / "task-u3" / "attempt-u3"
        self.assertFalse(rejected_path.exists())
        listed = self.run_git(repo, "worktree", "list", "--porcelain")
        self.assertEqual(listed.returncode, 0)
        self.assertNotIn(str(rejected_path), listed.stdout)

    def test_prestart_ownership_failure_exactly_cleans_nonrepo_workspace(self) -> None:
        fence = LeaseFence("task-prestart-clean", "attempt-prestart-clean", "lease", "worker", "instance", 1)
        failure = ExecutionBlocked("workspace_ownership_unavailable", "owner persistence failed")
        with mock.patch("scripts.task_agent_execution._write_workspace_ownership", side_effect=failure):
            with self.assertRaises(ExecutionBlocked) as blocked:
                prepare_workspace(
                    {"execution_kind": "non_repo"},
                    fence,
                    self.profile("local-model"),
                    worktree_root=self.worktree_root,
                )
        self.assertIs(blocked.exception, failure)
        self.assertFalse((self.worktree_root / fence.task_id / fence.attempt_id).exists())
        self.assertFalse((self.worktree_root / fence.task_id / f"{fence.attempt_id}.owner.json").exists())

    def test_prestart_ownership_failure_quarantines_with_recoverable_owner_evidence(self) -> None:
        fence = LeaseFence("task-prestart-quarantine", "attempt-prestart-quarantine", "lease", "worker", "instance", 1)
        failure = ExecutionBlocked("workspace_ownership_unavailable", "owner persistence failed")
        with (
            mock.patch("scripts.task_agent_execution._write_workspace_ownership", side_effect=failure),
            mock.patch(
                "scripts.task_agent_execution._remove_exact_owned_directory",
                return_value={"verified": False, "reason": "prestart_workspace_cleanup_failed"},
            ),
        ):
            with self.assertRaises(ExecutionBlocked) as blocked:
                prepare_workspace(
                    {"execution_kind": "non_repo"},
                    fence,
                    self.profile("local-model"),
                    worktree_root=self.worktree_root,
                )
        self.assertEqual(blocked.exception.reason, "quarantined")
        self.assertTrue(blocked.exception.evidence["owner"]["recoverable"])
        self.assertFalse(blocked.exception.evidence["cleanup"]["workspace"]["verified"])
        self.assertTrue((self.worktree_root / fence.task_id / fence.attempt_id).exists())

    def test_prestart_coding_owner_failure_does_not_ignore_git_removal_status(self) -> None:
        repo, base = self.git_repo()
        fence = LeaseFence("task-prestart-git", "attempt-prestart-git", "lease", "worker", "instance", 1)
        profile = self.profile("local-coding")
        real_git = task_execution._git

        def fail_remove(repo_path: Path, *args: str) -> subprocess.CompletedProcess[str]:
            if args[:2] == ("worktree", "remove"):
                return subprocess.CompletedProcess(["git", *args], 1, "", "forced removal failed")
            return real_git(repo_path, *args)

        with (
            mock.patch("scripts.task_agent_execution._write_workspace_ownership", side_effect=ExecutionBlocked("workspace_ownership_unavailable")),
            mock.patch("scripts.task_agent_execution._git", side_effect=fail_remove),
        ):
            with self.assertRaises(ExecutionBlocked) as blocked:
                prepare_workspace(
                    {"execution_kind": "coding", "repo": str(repo), "base_sha": base},
                    fence,
                    profile,
                    source_repo=repo,
                    worktree_root=self.worktree_root,
                )
        self.assertEqual(blocked.exception.reason, "quarantined")
        self.assertEqual(blocked.exception.evidence["cleanup"]["workspace"]["git_remove_returncode"], 1)
        self.assertTrue(blocked.exception.evidence["owner"]["recoverable"])

    def test_bounded_utf8_redacts_malicious_output(self) -> None:
        value = "é" * 100 + " Bearer " + "A" * 40
        bounded = bounded_utf8(value, 32)
        self.assertLessEqual(len(bounded.encode("utf-8")), 32)
        self.assertNotIn("Bearer A", bounded)
        self.assertIn("truncated", bounded)

    def test_canonical_redactor_covers_short_structured_multiline_and_local_paths(self) -> None:
        local_path = str(self.temp_dir / "private" / "file.txt")
        opaque_digest = "sha256:" + "a" * 64
        raw_opaque_digest = "sha256:" + "d" * 64
        workspace_ref = "workspace-" + "b" * 32
        authorization_ref = "authorization-" + "c" * 24
        payload = "\n".join(
            (
                "API_KEY=short-secret",
                "OPENAI_API_KEY=vendor-secret",
                "token=q7",
                "password=abc123",
                '{"client_secret":"tiny","nested":{"auth_token":"token-value","token":"xy"}}',
                "Authorization: Basic dXNlcjpwYXNz",
                "Cookie: session=short",
                "-----BEGIN PRIVATE KEY-----\nabc123\n-----END PRIVATE KEY-----",
                local_path,
                "/opt/local/bin/helper",
                "/var/local/task-state",
                "/Applications/Local Runner.app/Contents/MacOS/runner",
                "/home/local-user/.config/tool",
                "/Users/example/private.txt",
                "/Volumes/example/private.txt",
                "/custom-root/private.txt",
                "file:///opt/local/private.txt",
                r"C:\Users\example\private.txt",
                r"\\server\share\private.txt",
                "file:///C:/Users/example/private.txt",
                raw_opaque_digest,
                workspace_ref,
                "__CL_SENTINEL_0__",
                f"authorization_id={authorization_ref}",
                f"authorization_digest={opaque_digest}",
                "authorization_id=not-an-opaque-ref",
            )
        )
        redacted = redact_text(payload)
        for secret in (
            "short-secret",
            "vendor-secret",
            "q7",
            "abc123",
            "tiny",
            "token-value",
            "xy",
            "dXNlcjpwYXNz",
            "session=short",
        ):
            self.assertNotIn(secret, redacted)
        for path in (
            local_path,
            "/opt/local/bin/helper",
            "/var/local/task-state",
            "/Applications/Local Runner.app/Contents/MacOS/runner",
            "/home/local-user/.config/tool",
            "/Users/example/private.txt",
            "/Volumes/example/private.txt",
            "/custom-root/private.txt",
            "file:///opt/local/private.txt",
            r"C:\Users\example\private.txt",
            r"\\server\share\private.txt",
            "file:///C:/Users/example/private.txt",
        ):
            self.assertNotIn(path, redacted)
        self.assertIn("[LOCAL_PATH]", redacted)
        self.assertIn("[REDACTED_PEM]", redacted)
        self.assertNotIn(raw_opaque_digest, redacted)
        self.assertNotIn(workspace_ref, redacted)
        self.assertIn(authorization_ref, redacted)
        self.assertIn(opaque_digest, redacted)
        self.assertEqual(redacted.count(authorization_ref), 1)
        self.assertEqual(redacted.count(opaque_digest), 1)
        self.assertIn("__CL_SENTINEL_0__", redacted)
        self.assertIn("[REDACTED_TOKEN]", redacted)
        self.assertNotIn("not-an-opaque-ref", redacted)

        structured = redact_public_value(
            {
                "token": "q",
                "password_digest": opaque_digest,
                "token_impact": {"provider_total_tokens": 7},
                "artifact_digest": opaque_digest,
                "artifact_digests": [opaque_digest, "sha256:" + "e" * 64],
                "workspace_ref": workspace_ref,
                "authorization_id": authorization_ref,
                "authorization_digest": opaque_digest,
                "nested": {"authorization_id": "not-an-opaque-ref"},
                "serialized": json.dumps(
                    {
                        "password": "nested-secret",
                        "result_id": "result-structured",
                        "path": r"C:\Users\nested\credential.json",
                    }
                ),
                "sequence": (b"token=bytes-secret", {"password": "set-secret"}),
                "/Users/example/secret-key": "safe-value",
            }
        )
        self.assertEqual(structured["token"], "[REDACTED]")
        self.assertEqual(structured["password_digest"], "[REDACTED]")
        self.assertEqual(structured["token_impact"]["provider_total_tokens"], 7)
        self.assertEqual(structured["artifact_digest"], opaque_digest)
        self.assertEqual(structured["artifact_digests"][0], opaque_digest)
        self.assertEqual(structured["workspace_ref"], workspace_ref)
        self.assertEqual(structured["authorization_id"], authorization_ref)
        self.assertEqual(structured["authorization_digest"], opaque_digest)
        self.assertEqual(structured["nested"]["authorization_id"], "[REDACTED]")
        serialized = json.loads(structured["serialized"])
        self.assertEqual(serialized["password"], "[REDACTED]")
        self.assertEqual(serialized["result_id"], "result-structured")
        self.assertEqual(serialized["path"], "[LOCAL_PATH]")
        self.assertNotIn("bytes-secret", json.dumps(structured["sequence"]))
        self.assertNotIn("set-secret", json.dumps(structured["sequence"]))
        self.assertIn("[LOCAL_PATH]", structured)

        structural = redact_public_value({"signals": {"value": "A" * 32}, "token": "continuation-value"})
        self.assertEqual(structural["signals"], "[REDACTED]")
        self.assertEqual(structural["token"], "[REDACTED]")

        advisor = redact_public_value(
            {
                "schema_id": "run_advisor.v1",
                "objective_coherence": {
                    "signals": {
                        "mission_present": True,
                        "subobjective_count": 2,
                        "shared_terms": ["bounded", "evidence"],
                        "value": "A" * 32,
                    }
                },
                "graph_quality": {
                    "signals": {
                        "edge_samples": 3,
                        "relations": {"supports": 2},
                        "value": "A" * 32,
                    }
                },
            }
        )
        self.assertEqual(
            advisor["objective_coherence"]["signals"],
            {"mission_present": True, "subobjective_count": 2, "shared_terms": ["bounded", "evidence"]},
        )
        self.assertEqual(
            advisor["graph_quality"]["signals"],
            {"edge_samples": 3, "relations": {"supports": 2}},
        )

    def test_redaction_scans_secret_tails_before_absolute_path_collapse(self) -> None:
        probes = (
            "/opt/private-----BEGIN PRIVATE KEY-----\nPEM-LEAKME\n-----END PRIVATE KEY-----",
            '/opt/private{"password":"JSON-LEAKME","token":"TOKEN-LEAKME"}\nnext',
            "/var/private API_KEY=ENV-LEAKME\nnext",
            "/Applications/private Authorization: Basic HEADER-LEAKME\nnext",
        )
        redacted = tuple(redact_text(probe) for probe in probes)
        rendered = "\n".join(redacted)
        for secret in ("PEM-LEAKME", "JSON-LEAKME", "TOKEN-LEAKME", "ENV-LEAKME", "HEADER-LEAKME"):
            self.assertNotIn(secret, rendered)
        self.assertNotIn("-----END PRIVATE KEY-----", rendered)
        self.assertTrue(all("[LOCAL_PATH]" in item for item in redacted))

    def test_stdout_stderr_manifest_and_blocked_errors_share_canonical_scanner(self) -> None:
        local_path = str(self.temp_dir / "private" / "credential.json")
        capture = CaptureResult(
            0,
            f"API_KEY=short-secret\n{local_path}\n".encode(),
            b"password=abc123\nAuthorization: Basic dXNlcjpwYXNz\n",
            False,
            False,
            False,
            "execution_observed",
            {"verified": True},
        )
        fence = LeaseFence("task-scan", "attempt-scan", "lease-scan", "worker", "instance", 1)
        artifacts = process_artifacts(capture, fence)
        serialized_artifacts = json.dumps(artifacts, sort_keys=True)
        for secret in ("short-secret", "abc123", "dXNlcjpwYXNz", local_path):
            self.assertNotIn(secret, serialized_artifacts)
        self.assertEqual([item["redaction_status"] for item in artifacts], ["worker_redacted", "worker_redacted"])
        self.assertTrue(all(str(item["redaction_receipt"]).startswith("sha256:") for item in artifacts))

        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-model",
            self.profile("local-model"),
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
            WorkspaceBinding("non_repo", self.worktree_root, None, None, ""),
            None,
            {},
            "",
        )
        manifest = result_manifest(
            prepared,
            "password=manifest-secret",
            '{"authorization":"Basic bWFuaWZlc3Q6c2VjcmV0"}',
            artifacts,
            "publication-scan",
        )
        serialized_manifest = json.dumps(manifest, sort_keys=True)
        self.assertNotIn("manifest-secret", serialized_manifest)
        self.assertNotIn("bWFuaWZlc3Q6c2VjcmV0", serialized_manifest)

        blocked = ExecutionBlocked(
            "credential_failure",
            f"API_KEY=error-secret at {local_path}",
            evidence={"password": "evidence-secret", "path": local_path},
        )
        serialized_error = json.dumps(
            {"reason": blocked.reason, "detail": blocked.detail, "evidence": blocked.evidence},
            sort_keys=True,
        )
        for secret in ("error-secret", "evidence-secret", local_path):
            self.assertNotIn(secret, serialized_error)

    def test_redaction_normalizes_sensitive_keys_urls_nested_encoding_and_public_sinks(self) -> None:
        leaks = (
            "U3-CLIENT-SECRET-LEAK",
            "U3-REFRESH-TOKEN-LEAK",
            "U3-API-TOKEN-LEAK",
            "U3-API-SECRET-LEAK",
            "U3-SECRET-KEY-LEAK",
            "U3-PRIVATE-KEY-LEAK",
            "U3-AUTHORIZATION-LEAK",
            "U3-COOKIE-LEAK",
            "U3-USERINFO-LEAK",
        )
        nested = json.dumps({"client_secret": leaks[0], "child": {"refresh_token": leaks[1]}})
        from urllib.parse import quote

        encoded_nested = quote(nested, safe="")
        text = (
            "client_secret=" + leaks[0] + " refresh-token=" + leaks[1] + " api-token=" + leaks[2] + "\n"
            "api_secret=" + leaks[3] + " secret_key=" + leaks[4] + " private_key=" + leaks[5] + "\n"
            "Authorization: Bearer " + leaks[6] + " Cookie: session=" + leaks[7] + "\n"
            "https://user:" + leaks[8] + "@example.invalid/?refresh%5Ftoken=" + leaks[1] + "&api-token=" + leaks[2] + "\n"
            "payload=" + encoded_nested
        )
        redacted = redact_text(text)
        for leak in leaks:
            self.assertNotIn(leak, redacted)

        structured = redact_public_value(
            {
                "client_secret": leaks[0],
                "refresh-token": leaks[1],
                "api-token": leaks[2],
                "api_secret": leaks[3],
                "secret_key": leaks[4],
                "private_key": leaks[5],
                "authorization": leaks[6],
                "cookie": leaks[7],
                "userinfo": leaks[8],
                "nested": {"encoded": encoded_nested},
                "authorization_id": "authorization-public-u3",
            }
        )
        serialized = json.dumps(structured, sort_keys=True)
        for leak in leaks:
            self.assertNotIn(leak, serialized)
        self.assertEqual(structured["authorization_id"], "authorization-public-u3")

        fence = LeaseFence("task-redaction-sinks", "attempt-redaction-sinks", "lease", "worker", "instance", 1)
        session = SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {})
        event = task_execution._event_payload(fence, session, "task.test", "summary client_secret=" + leaks[0], structured)
        self.assertNotIn(leaks[0], json.dumps(event, sort_keys=True))
        self.assertNotIn(leaks[8], json.dumps(fenced_payload(fence, {"metadata": structured}), sort_keys=True))
        artifact = task_execution.artifact_ref("stdout.txt", text, task_id=fence.task_id, attempt_id=fence.attempt_id)
        self.assertNotIn(leaks[0], json.dumps(artifact, sort_keys=True))
        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-model",
            self.profile("local-model"),
            session,
            WorkspaceBinding("non_repo", self.worktree_root / fence.task_id / fence.attempt_id, None, None, ""),
            None,
            {},
            "",
        )
        manifest = result_manifest(prepared, "summary", "public output", [{"metadata": structured}], "publication-u3")
        self.assertNotIn(leaks[3], json.dumps(manifest, sort_keys=True))

    def test_redaction_deny_defaults_generic_secret_variants_across_bytes_artifacts_and_inference(self) -> None:
        from urllib.parse import quote

        variants = {
            "key": "U3-KEY-VARIANT",
            "api": "U3-API-VARIANT",
            "api_value": "U3-API-VARIANT-KEY",
            "auth": "U3-AUTH-VARIANT",
            "session": "U3-SESSION-VARIANT",
            "sig": "U3-SIG-VARIANT",
            "pem": "U3-PEM-VARIANT",
        }
        nested = json.dumps({"nested": variants, "sequence": [variants, "safe"]})
        encoded = quote(nested, safe="")
        text = " ".join(f"{key}={value}" for key, value in variants.items()) + f" payload={encoded}"
        redacted = redact_text(text)
        for value in variants.values():
            self.assertNotIn(value, redacted)
        structured = redact_public_value(
            {
                **variants,
                "nested": variants,
                "bytes": b"key=U3-BYTES-VARIANT session=U3-BYTES-SESSION",
                "sequence": [b"api=U3-SEQUENCE-VARIANT", {"pem": "U3-SEQUENCE-PEM"}],
            }
        )
        serialized = json.dumps(structured, sort_keys=True)
        for value in (*variants.values(), "U3-BYTES-VARIANT", "U3-BYTES-SESSION", "U3-SEQUENCE-VARIANT", "U3-SEQUENCE-PEM"):
            self.assertNotIn(value, serialized)
        with self.assertRaises(ExecutionBlocked) as artifact_blocked:
            artifact_ref_bytes(
                "artifact-ref-bytes.txt",
                b"key=U3-ARTIFACT-BYTES",
                task_id="task-redaction-variants",
                attempt_id="attempt-redaction-variants",
                media_type="text/plain; charset=utf-8",
                max_bytes=4096,
            )
        self.assertEqual(artifact_blocked.exception.reason, "secret_in_result")
        for output in (
            "key=U3-INFERENCE-KEY",
            "api=U3-INFERENCE-API",
            "api_value=U3-INFERENCE-API-VALUE",
            "auth=U3-INFERENCE-AUTH",
            "session=U3-INFERENCE-SESSION",
            "sig=U3-INFERENCE-SIG",
            "pem=U3-INFERENCE-PEM",
        ):
            with self.subTest(output=output):
                with self.assertRaises(ExecutionBlocked) as inference_blocked:
                    validate_inference_result(output, {"provider": "local"})
                self.assertEqual(inference_blocked.exception.reason, "inference_result_invalid")

    def test_fence_and_publication_ack_are_explicit(self) -> None:
        fence = LeaseFence("task", "attempt", "lease", "worker", "instance", 7)
        payload = fenced_payload(fence, {"session_id": "session", "context_snapshot_id": "snapshot", "context_pack_hash": "hash"})
        self.assertEqual(payload["fence"], fence.as_dict())
        self.assertEqual(payload["generation"], 7)
        self.assertFalse(publication_acknowledged({"ok": True, "acknowledged": True}))

        workspace = WorkspaceBinding("non_repo", self.worktree_root, None, None, "")
        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-model",
            self.profile("local-model"),
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        from scripts.task_agent_execution import result_manifest

        manifest = result_manifest(prepared, "observed", "", [], "publication-test")
        response = self.publication_response(prepared, manifest)
        self.assertTrue(
            publication_acknowledged(
                response,
                fence=fence,
                publication_id="publication-test",
                result_id=str(manifest["result_id"]),
                workspace_ref=str(manifest["workspace"]["workspace_ref"]),  # type: ignore[index]
                idempotency_key=f"task-result:{manifest['result_id']}",
            )
        )
        foreign = copy.deepcopy(response)
        foreign["publication_receipt"]["attempt_id"] = "foreign"  # type: ignore[index]
        self.assertFalse(
            publication_acknowledged(
                foreign,
                fence=fence,
                publication_id="publication-test",
                result_id=str(manifest["result_id"]),
                workspace_ref=str(manifest["workspace"]["workspace_ref"]),  # type: ignore[index]
                idempotency_key=f"task-result:{manifest['result_id']}",
            )
        )
        foreign_publication = copy.deepcopy(response)
        foreign_publication["publication"]["worker_instance_id"] = "foreign-instance"  # type: ignore[index]
        self.assertFalse(
            publication_acknowledged(
                foreign_publication,
                fence=fence,
                publication_id="publication-test",
                result_id=str(manifest["result_id"]),
                workspace_ref=str(manifest["workspace"]["workspace_ref"]),  # type: ignore[index]
                idempotency_key=f"task-result:{manifest['result_id']}",
            )
        )
        inconsistent = copy.deepcopy(response)
        inconsistent["publication"]["status"] = "committed"  # type: ignore[index]
        inconsistent["publication"]["writeback_status"] = "committed"  # type: ignore[index]
        inconsistent["publication_receipt"]["state"] = "committed"  # type: ignore[index]
        inconsistent["publication_receipt"]["receipt_digest"] = self.receipt_digest(  # type: ignore[index]
            inconsistent["publication_receipt"], "receipt_digest"  # type: ignore[arg-type,index]
        )
        self.assertFalse(
            publication_acknowledged(
                inconsistent,
                fence=fence,
                publication_id="publication-test",
                result_id=str(manifest["result_id"]),
                workspace_ref=str(manifest["workspace"]["workspace_ref"]),  # type: ignore[index]
                idempotency_key=f"task-result:{manifest['result_id']}",
            )
        )

    def test_post_publication_replay_accepts_exact_durable_matrix_with_staged_receipt(self) -> None:
        prepared = self.prepared_coding()
        manifest = result_manifest(prepared, "observed", "", [], "publication-post-replay")
        expected_pairs = (
            ("writeback_pending", "pending"),
            ("writeback_failed", "failed"),
            ("committed", "committed"),
            ("dead_letter", "dead_letter"),
        )
        for status, writeback_status in expected_pairs:
            with self.subTest(status=status, writeback_status=writeback_status):
                response = self.route_publication_response(
                    prepared,
                    manifest,
                    status=status,
                    writeback_status=writeback_status,
                )
                publication, receipt, authorization = validate_publication_receipt(
                    response,
                    fence=prepared.fence,
                    publication_id="publication-post-replay",
                    result_id=str(manifest["result_id"]),
                    workspace_ref=str(manifest["workspace"]["workspace_ref"]),  # type: ignore[index]
                    idempotency_key=f"task-result:{manifest['result_id']}",
                )
                self.assertEqual((publication["status"], publication["writeback_status"]), (status, writeback_status))
                self.assertEqual(receipt["state"], "staged")
                self.assertEqual(authorization["state"], "authorized")

        mismatched = self.route_publication_response(
            prepared,
            manifest,
            status="committed",
            writeback_status="pending",
        )
        with self.assertRaises(ExecutionBlocked):
            validate_publication_receipt(
                mismatched,
                fence=prepared.fence,
                publication_id="publication-post-replay",
                result_id=str(manifest["result_id"]),
                workspace_ref=str(manifest["workspace"]["workspace_ref"]),  # type: ignore[index]
                idempotency_key=f"task-result:{manifest['result_id']}",
            )

        invalid = self.route_publication_response(
            prepared,
            manifest,
            status="committed",
            writeback_status="committed",
        )
        invalid["publication_receipt"]["state"] = "committed"  # type: ignore[index]
        invalid["publication_receipt"]["receipt_digest"] = self.receipt_digest(  # type: ignore[index]
            invalid["publication_receipt"], "receipt_digest"  # type: ignore[arg-type,index]
        )
        with self.assertRaises(ExecutionBlocked):
            validate_publication_receipt(
                invalid,
                fence=prepared.fence,
                publication_id="publication-post-replay",
                result_id=str(manifest["result_id"]),
                workspace_ref=str(manifest["workspace"]["workspace_ref"]),  # type: ignore[index]
                idempotency_key=f"task-result:{manifest['result_id']}",
            )

    def test_attempt_publication_consumer_rejects_wrapped_unknown_and_foreign_generation_shapes(self) -> None:
        prepared = self.prepared_coding()
        publication_id = "publication-attempt-consumer"
        manifest = result_manifest(prepared, "observed", "", [], publication_id)
        wrapped = self.publication_response(prepared, manifest)
        top_level = self.restart_publication_response(wrapped)
        calls: list[tuple[str, object]] = []

        def fetch(response: dict[str, object]) -> tuple[dict[str, object], dict[str, object], dict[str, object]]:
            def get_json(_base: str, path: str, **kwargs: object) -> dict[str, object]:
                calls.append((path, copy.deepcopy(kwargs.get("params"))))
                return copy.deepcopy(response)

            return fetch_attempt_publication_receipt(
                "http://gateway.invalid",
                fence=prepared.fence,
                publication_id=publication_id,
                result_id=str(manifest["result_id"]),
                workspace_ref=str(manifest["workspace"]["workspace_ref"]),  # type: ignore[index]
                get_json=get_json,
            )

        publication, _receipt, _authorization = fetch(top_level)
        self.assertEqual(publication["publication_id"], publication_id)
        self.assertEqual(
            calls[-1],
            (
                f"/agents/tasks/{prepared.fence.task_id}/attempts/{prepared.fence.attempt_id}/publication",
                {
                    "lease_id": prepared.fence.lease_id,
                    "generation": str(prepared.fence.generation),
                    "worker_id": prepared.fence.worker_id,
                    "worker_instance_id": prepared.fence.worker_instance_id,
                    "idempotency_key": f"task-result:{manifest['result_id']}",
                },
            ),
        )

        bad_shapes = (wrapped, {**top_level, "unexpected": True}, {**top_level, "lease_generation": 999})
        for response in bad_shapes:
            with self.subTest(keys=sorted(response)):
                with self.assertRaises(ExecutionBlocked) as blocked:
                    fetch(response)
                self.assertEqual(blocked.exception.reason, "publication_receipt_invalid")

    def test_task_environment_never_inherits_host_credentials(self) -> None:
        fence = LeaseFence("task", "attempt", "lease", "worker", "instance", 1)
        snapshot = SnapshotBinding("snapshot", "hash", "session", "task", "attempt", {"contextPack": {}})
        workspace = WorkspaceBinding("non_repo", self.worktree_root, None, None, "")
        env = build_execution_env(
            {"project": "p", "payload": {}},
            snapshot,
            workspace,
            fence,
            self.profile("local-model"),
            base_env={"PATH": "/usr/bin", "HOME": "/host-home", "OPENAI_API_KEY": "host-value", "AWS_SECRET_ACCESS_KEY": "host-value"},
        )
        self.assertEqual(env["HOME"], str(self.worktree_root / ".home"))
        self.assertNotIn("OPENAI_API_KEY", env)
        self.assertNotIn("AWS_SECRET_ACCESS_KEY", env)
        self.assertNotIn("SSH_AUTH_SOCK", env)

    def test_context_unavailable_has_no_local_fallback(self) -> None:
        claim = self.claim()

        def unavailable(*_args: object, **_kwargs: object) -> dict[str, object]:
            raise OSError("gateway unavailable")

        with self.assertRaises(ExecutionBlocked) as blocked:
            from scripts.task_agent_execution import prepare_execution

            prepare_execution(claim, worker="worker-u3", orchestrator_url="http://gateway.invalid", get_json=unavailable, worktree_root=self.worktree_root)
        self.assertEqual(blocked.exception.reason, "context_unavailable")

    def test_coding_result_truth_and_opaque_manifest_surface(self) -> None:
        repo, base = self.git_repo()
        fence = LeaseFence("task-result", "attempt-result", "lease", "worker", "instance", 1)
        profile = self.profile("local-coding")
        workspace = prepare_workspace({"execution_kind": "coding", "repo": str(repo), "base_sha": base}, fence, profile, source_repo=repo, worktree_root=self.worktree_root)
        (workspace.cwd / "README.md").write_text("changed\n", encoding="utf-8")
        prepared = PreparedExecution(
            {"id": fence.task_id, "payload": {}},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-coding",
            profile,
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {"contextPack": {}}),
            workspace,
            None,
            {},
            "",
        )
        truth, artifacts = collect_coding_result(prepared)
        self.assertEqual(truth["base_sha"], base)
        self.assertEqual(truth["final_head"], base)
        self.assertIn("README.md", truth["changed_paths"])
        self.assertTrue(truth["diff_digest"].startswith("sha256:"))
        self.assertTrue(truth["patch_applies_to_base"])
        self.assertEqual(truth["verified_tree"], truth["final_tree"])
        self.assertEqual(artifacts[0]["media_type"], "application/vnd.git.patch")
        self.assertEqual(artifacts[0]["digest"], truth["diff_digest"])
        from scripts.task_agent_execution import result_manifest

        manifest = result_manifest(prepared, "observed", "", artifacts, "publication")
        rendered = json.dumps(manifest)
        self.assertNotIn(str(workspace.cwd), rendered)
        self.assertEqual(manifest["cleanup"]["owner"], "gateway_publication_worker")

        profile["resource_limits"]["max_file_bytes"] = 1  # type: ignore[index]
        with self.assertRaises(ExecutionBlocked) as oversize:
            collect_coding_result(prepared)
        self.assertEqual(oversize.exception.reason, "artifact_oversize")
        profile["resource_limits"]["max_file_bytes"] = 16777216  # type: ignore[index]

        (workspace.cwd / "secret.txt").write_text("API_KEY=restricted-value\n", encoding="utf-8")
        with self.assertRaises(ExecutionBlocked) as secret:
            collect_coding_result(prepared)
        self.assertEqual(secret.exception.reason, "secret_in_result")

    def test_bounded_git_overflow_uses_attested_group_termination(self) -> None:
        fake_proc = mock.Mock()
        fake_proc.pid = 424248
        fake_proc.poll.return_value = None
        fake_proc.stdout.read.return_value = b"overflow"
        fake_proc.communicate.side_effect = [subprocess.TimeoutExpired("git", 1), (b"", b"")]
        repo = self.new_root("bounded-git-overflow")
        with (
            mock.patch("scripts.task_agent_execution._executable_inode", return_value=123),
            mock.patch(
                "scripts.task_agent_execution.subprocess.Popen",
                return_value=fake_proc,
            ) as popen,
            mock.patch(
                "scripts.task_agent_execution.terminate_owned_process_group",
                return_value={"verified": True, "quarantined": False, "reason": "terminated"},
            ) as terminate,
        ):
            with self.assertRaises(ExecutionBlocked) as blocked:
                task_execution._git_capture_bounded(
                    repo,
                    ["status"],
                    max_bytes=3,
                    reason="coding_result_unavailable",
                )
        self.assertEqual(blocked.exception.reason, "artifact_oversize")
        self.assertEqual(popen.call_args.kwargs["start_new_session"], True)
        terminate.assert_called_once_with(
            fake_proc,
            expected_executable=str(task_execution.GIT_EXECUTABLE),
            expected_cwd=repo,
            term_timeout=1.0,
            known_pgid=fake_proc.pid,
            tracked_pids={fake_proc.pid},
            expected_executable_inode=123,
        )
        fake_proc.kill.assert_not_called()

    def test_committed_only_result_patch_is_complete_and_does_not_mutate_indexes_or_objects(self) -> None:
        repo, base = self.git_repo()
        fence = LeaseFence("task-committed", "attempt-committed", "lease", "worker", "instance", 1)
        profile = self.profile("local-coding")
        workspace = prepare_workspace(
            {"execution_kind": "coding", "repo": str(repo), "base_sha": base},
            fence,
            profile,
            source_repo=repo,
            worktree_root=self.worktree_root,
        )
        (workspace.cwd / "README.md").write_text("committed-only\n", encoding="utf-8")
        self.assertEqual(self.run_git(workspace.cwd, "add", "README.md").returncode, 0)
        self.assertEqual(self.run_git(workspace.cwd, "commit", "-qm", "committed result").returncode, 0)
        index_before = self.run_git(workspace.cwd, "diff", "--cached", "--binary").stdout
        source_index_before = self.run_git(repo, "diff", "--cached", "--binary").stdout
        common_dir = Path(self.run_git(repo, "rev-parse", "--git-common-dir").stdout.strip())
        if not common_dir.is_absolute():
            common_dir = (repo / common_dir).resolve()
        object_files_before = {path.relative_to(common_dir) for path in (common_dir / "objects").rglob("*") if path.is_file()}
        prepared = PreparedExecution(
            {"id": fence.task_id, "payload": {}},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-coding",
            profile,
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {"contextPack": {}}),
            workspace,
            None,
            {},
            "",
        )

        truth, artifacts = collect_coding_result(prepared)

        patch = base64.b64decode(artifacts[0]["content_base64"])
        self.assertNotEqual(truth["final_head"], base)
        self.assertIn(b"committed-only", patch)
        self.assertTrue(truth["patch_applies_to_base"])
        self.assertEqual(truth["verified_tree"], truth["final_tree"])
        self.assertEqual(self.run_git(workspace.cwd, "diff", "--cached", "--binary").stdout, index_before)
        self.assertEqual(self.run_git(repo, "diff", "--cached", "--binary").stdout, source_index_before)
        object_files_after = {path.relative_to(common_dir) for path in (common_dir / "objects").rglob("*") if path.is_file()}
        self.assertEqual(object_files_after, object_files_before)

    def test_untracked_only_result_patch_contains_file_content(self) -> None:
        repo, base = self.git_repo()
        fence = LeaseFence("task-untracked", "attempt-untracked", "lease", "worker", "instance", 1)
        profile = self.profile("local-coding")
        workspace = prepare_workspace(
            {"execution_kind": "coding", "repo": str(repo), "base_sha": base},
            fence,
            profile,
            source_repo=repo,
            worktree_root=self.worktree_root,
        )
        (workspace.cwd / "untracked-result.txt").write_text("untracked-result-content\n", encoding="utf-8")
        prepared = PreparedExecution(
            {"id": fence.task_id, "payload": {}},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-coding",
            profile,
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {"contextPack": {}}),
            workspace,
            None,
            {},
            "",
        )

        truth, artifacts = collect_coding_result(prepared)

        patch = base64.b64decode(artifacts[0]["content_base64"])
        self.assertIn("untracked-result.txt", truth["untracked_paths"])
        self.assertIn(b"new file mode", patch)
        self.assertIn(b"untracked-result-content", patch)
        self.assertTrue(truth["patch_applies_to_base"])

    def test_result_patch_verifies_deletion_rename_and_binary_state(self) -> None:
        repo, _initial = self.git_repo()
        (repo / "delete-me.txt").write_text("delete this\n", encoding="utf-8")
        (repo / "rename-me.txt").write_text("rename this\n", encoding="utf-8")
        self.assertEqual(self.run_git(repo, "add", "delete-me.txt", "rename-me.txt").returncode, 0)
        self.assertEqual(self.run_git(repo, "commit", "-qm", "artifact cases").returncode, 0)
        base = self.run_git(repo, "rev-parse", "HEAD").stdout.strip()
        fence = LeaseFence("task-artifacts", "attempt-artifacts", "lease", "worker", "instance", 1)
        profile = self.profile("local-coding")
        workspace = prepare_workspace(
            {"execution_kind": "coding", "repo": str(repo), "base_sha": base},
            fence,
            profile,
            source_repo=repo,
            worktree_root=self.worktree_root,
        )
        (workspace.cwd / "delete-me.txt").unlink()
        self.assertEqual(self.run_git(workspace.cwd, "mv", "rename-me.txt", "renamed.txt").returncode, 0)
        (workspace.cwd / "binary-result.bin").write_bytes(b"\x00\x01\x02binary\xff\x00result\n")
        prepared = PreparedExecution(
            {"id": fence.task_id, "payload": {}},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-coding",
            profile,
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {"contextPack": {}}),
            workspace,
            None,
            {},
            "",
        )

        truth, artifacts = collect_coding_result(prepared)

        patch = base64.b64decode(artifacts[0]["content_base64"])
        self.assertIn(b"deleted file mode", patch)
        self.assertIn(b"rename from rename-me.txt", patch)
        self.assertIn(b"rename to renamed.txt", patch)
        self.assertIn(b"GIT binary patch", patch)
        self.assertIn("delete-me.txt", truth["changed_paths"])
        self.assertIn("renamed.txt", truth["changed_paths"])
        self.assertIn("binary-result.bin", truth["changed_paths"])
        self.assertEqual(truth["verified_tree"], truth["final_tree"])

    def test_result_capture_cleanup_failure_cannot_publish_success(self) -> None:
        prepared = self.prepared_coding()
        (prepared.workspace.cwd / "capture-result.txt").write_text("captured\n", encoding="utf-8")
        retained: list[Path] = []
        real_rmtree = shutil.rmtree

        def retain_capture(target: object, *args: object, **kwargs: object) -> None:
            candidate = Path(target)
            if kwargs.get("dir_fd") is not None and candidate.name.startswith("capture-"):
                candidate = self.worktree_root / ".result-capture" / candidate.name
                retained.append(candidate)
                return None
            real_rmtree(target, *args, **kwargs)  # type: ignore[arg-type]

        try:
            with mock.patch("scripts.task_agent_execution.shutil.rmtree", side_effect=retain_capture):
                with self.assertRaises(ExecutionBlocked) as blocked:
                    collect_coding_result(prepared)
            self.assertEqual(blocked.exception.reason, "result_capture_cleanup_failed")
            self.assertEqual(len(retained), 1)
            self.assertTrue(retained[0].exists())
        finally:
            for capture_dir in retained:
                real_rmtree(capture_dir, ignore_errors=True)
            capture_root = self.worktree_root / ".result-capture"
            try:
                capture_root.rmdir()
            except OSError:
                pass

    def test_existing_runtime_and_result_capture_symlinks_or_files_are_rejected_before_write(self) -> None:
        outside = self.new_root("runtime-outside")
        runtime_root = self.worktree_root / ".runtime"
        result_root = self.worktree_root / ".result-capture"
        for root, reason in ((runtime_root, "runtime_root_identity_invalid"),):
            with self.subTest(root=root):
                root.symlink_to(outside, target_is_directory=True)
                with self.assertRaises(ExecutionBlocked) as blocked:
                    task_execution._helper_free_docker_config(self.worktree_root, "symlink-runtime")
                self.assertEqual(blocked.exception.reason, reason)
                self.assertFalse(any(outside.iterdir()))
                root.unlink()

        prepared = self.prepared_coding()
        result_root.write_text("not-a-directory", encoding="utf-8")
        with self.assertRaises(ExecutionBlocked) as blocked:
            collect_coding_result(prepared)
        self.assertEqual(blocked.exception.reason, "result_capture_root_identity_invalid")
        result_root.unlink()

        result_root.symlink_to(outside, target_is_directory=True)
        with self.assertRaises(ExecutionBlocked) as blocked:
            collect_coding_result(prepared)
        self.assertEqual(blocked.exception.reason, "result_capture_root_identity_invalid")
        self.assertFalse(any(outside.iterdir()))
        result_root.unlink()

    def test_cleanup_requires_exact_receipt_authorization_and_termination_proof(self) -> None:
        fence = LeaseFence("task-cleanup", "attempt-cleanup", "lease", "worker", "instance", 1)
        profile = self.profile("local-model")
        workspace = prepare_workspace({"execution_kind": "non_repo"}, fence, profile, worktree_root=self.worktree_root)
        prepared = PreparedExecution(
            {"id": fence.task_id, "payload": {}},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-model",
            profile,
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {"contextPack": {}}),
            workspace,
            None,
            {},
            "",
        )
        from scripts.task_agent_execution import result_manifest

        manifest = result_manifest(prepared, "observed", "", [], "publication")
        response = self.publication_response(prepared, manifest)
        nonterminal = copy.deepcopy(response["cleanup_authorization"])
        nonterminal["attempt_terminal"] = False  # type: ignore[index]
        nonterminal["authorization_digest"] = self.receipt_digest(  # type: ignore[index]
            nonterminal, "authorization_digest"  # type: ignore[arg-type]
        )
        with self.assertRaises(ExecutionBlocked) as nonterminal_cleanup:
            cleanup_workspace_after_receipt(
                prepared,
                nonterminal,  # type: ignore[arg-type]
                publication_receipt=response["publication_receipt"],  # type: ignore[arg-type]
                result_id=str(manifest["result_id"]),
                termination={"verified": True},
            )
        self.assertEqual(nonterminal_cleanup.exception.reason, "cleanup_authorization_invalid")
        self.assertTrue(workspace.cwd.exists())
        with self.assertRaises(ExecutionBlocked) as missing:
            cleanup_workspace_after_receipt(
                prepared,
                response["cleanup_authorization"],  # type: ignore[arg-type]
                publication_receipt=response["publication_receipt"],  # type: ignore[arg-type]
                result_id=str(manifest["result_id"]),
                termination={"verified": False},
            )
        self.assertEqual(missing.exception.reason, "cleanup_termination_unverified")
        self.assertTrue(workspace.cwd.exists())
        result = cleanup_workspace_after_receipt(
            prepared,
            response["cleanup_authorization"],  # type: ignore[arg-type]
            publication_receipt=response["publication_receipt"],  # type: ignore[arg-type]
            result_id=str(manifest["result_id"]),
            termination={"verified": True, "container": {"verified": True}},
        )
        self.assertEqual(result["state"], "cleaned")
        self.assertFalse(workspace.cwd.exists())
        marker = self.worktree_root / fence.task_id / f"{fence.attempt_id}.owner.json"
        self.assertTrue(marker.exists())

        def post_json(_base: str, _path: str, payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            receipt = copy.deepcopy(payload["cleanup_receipt"])
            receipt.update({"recorded": True, "durable": True, "acknowledged": True})  # type: ignore[union-attr]
            return {"cleanup_receipt": receipt}

        from scripts.task_agent_execution import report_cleanup_receipt

        recorded = report_cleanup_receipt(
            prepared,
            result,
            orchestrator_url="http://gateway.invalid",
            post_json=post_json,
        )
        self.assertTrue(recorded["recorded"])
        self.assertFalse(marker.exists())

    def test_failed_cleanup_receipt_report_is_retried_from_exact_owner_marker(self) -> None:
        fence = LeaseFence("task-cleanup-retry", "attempt-cleanup-retry", "lease", "worker-u3", "instance-u3", 2)
        workspace = prepare_workspace(
            {"execution_kind": "non_repo"},
            fence,
            self.profile("local-model"),
            worktree_root=self.worktree_root,
        )
        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-model",
            self.profile("local-model"),
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        manifest = result_manifest(prepared, "observed", "", [], "publication-" + hashlib.sha256(
            f"{fence.task_id}\0{fence.attempt_id}\0publication".encode()
        ).hexdigest()[:32])
        response = self.publication_response(prepared, manifest)
        receipt = cleanup_workspace_after_receipt(
            prepared,
            response["cleanup_authorization"],  # type: ignore[arg-type]
            publication_receipt=response["publication_receipt"],  # type: ignore[arg-type]
            result_id=str(manifest["result_id"]),
            termination={"verified": True},
        )
        marker = self.worktree_root / fence.task_id / f"{fence.attempt_id}.owner.json"
        self.assertFalse(workspace.cwd.exists())
        self.assertTrue(marker.exists())

        from scripts.task_agent_execution import report_cleanup_receipt

        with self.assertRaises(ExecutionBlocked):
            report_cleanup_receipt(
                prepared,
                receipt,
                orchestrator_url="http://gateway.invalid",
                post_json=lambda *_args, **_kwargs: (_ for _ in ()).throw(OSError("API_KEY=short-secret")),
            )
        self.assertTrue(marker.exists())

        def get_json(_base: str, _path: str, **_kwargs: object) -> dict[str, object]:
            return self.restart_publication_response(response)

        def post_json(_base: str, _path: str, payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            recorded = copy.deepcopy(payload["cleanup_receipt"])
            recorded.update({"recorded": True, "durable": True, "acknowledged": True})  # type: ignore[union-attr]
            return {"cleanup_receipt": recorded}

        with (
            mock.patch("scripts.task_agent_execution._workspace_processes_absent", return_value={"verified": True, "reason": "workspace_absent"}),
            mock.patch("scripts.task_agent_execution._workspace_containers_absent", return_value={"verified": True, "reason": "container_absent", "task_ref": "opaque"}),
        ):
            reconciled = reconcile_owned_workspaces(
                orchestrator_url="http://gateway.invalid",
                worker="worker-u3",
                worker_instance="instance-u3",
                get_json=get_json,
                post_json=post_json,
                worktree_root=self.worktree_root,
            )
        self.assertEqual(len(reconciled["cleaned"]), 1)
        self.assertFalse(marker.exists())

    def test_coding_cleanup_retains_owner_when_git_removal_status_is_failed(self) -> None:
        repo, base = self.git_repo()
        fence = LeaseFence("task-git-cleanup", "attempt-git-cleanup", "lease", "worker", "instance", 1)
        profile = self.profile("local-coding")
        workspace = prepare_workspace(
            {"execution_kind": "coding", "repo": str(repo), "base_sha": base},
            fence,
            profile,
            source_repo=repo,
            worktree_root=self.worktree_root,
        )
        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-coding",
            profile,
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        manifest = result_manifest(prepared, "observed", "", [], "publication")
        response = self.publication_response(prepared, manifest)
        failed_remove = subprocess.CompletedProcess(["git"], 1, "", "forced removal failed")
        listed = subprocess.CompletedProcess(["git"], 0, "", "")
        with mock.patch("scripts.task_agent_execution._git", side_effect=(failed_remove, listed)):
            with self.assertRaises(ExecutionBlocked) as blocked:
                cleanup_workspace_after_receipt(
                    prepared,
                    response["cleanup_authorization"],  # type: ignore[arg-type]
                    publication_receipt=response["publication_receipt"],  # type: ignore[arg-type]
                    result_id=str(manifest["result_id"]),
                    termination={"verified": True, "container": {"verified": True}},
                )
        self.assertEqual(blocked.exception.reason, "cleanup_failed")
        self.assertTrue(workspace.cwd.exists())
        self.assertTrue((self.worktree_root / fence.task_id / f"{fence.attempt_id}.owner.json").exists())

    def test_coding_cleanup_accepts_idempotent_rc128_after_exact_absence_proof(self) -> None:
        repo, base = self.git_repo()
        fence = LeaseFence("task-git-retry", "attempt-git-retry", "lease", "worker", "instance", 1)
        profile = self.profile("local-coding")
        workspace = prepare_workspace(
            {"execution_kind": "coding", "repo": str(repo), "base_sha": base},
            fence,
            profile,
            source_repo=repo,
            worktree_root=self.worktree_root,
        )
        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-coding",
            profile,
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        manifest = result_manifest(prepared, "observed", "", [], "publication")
        response = self.publication_response(prepared, manifest)
        shutil.rmtree(workspace.cwd)
        remove_retry = subprocess.CompletedProcess(["git"], 128, "", "already removed")
        listed = subprocess.CompletedProcess(["git"], 0, "", "")
        with mock.patch("scripts.task_agent_execution._git", side_effect=(remove_retry, listed)):
            receipt = cleanup_workspace_after_receipt(
                prepared,
                response["cleanup_authorization"],  # type: ignore[arg-type]
                publication_receipt=response["publication_receipt"],  # type: ignore[arg-type]
                result_id=str(manifest["result_id"]),
                termination={"verified": True, "container": {"verified": True}},
            )
        self.assertEqual(receipt["state"], "cleaned")
        self.assertFalse(workspace.cwd.exists())

    def test_startup_reconciliation_cleans_only_exact_receipt_owned_absent_attempt(self) -> None:
        fence = LeaseFence("task-orphan", "attempt-orphan", "lease", "worker-u3", "instance-u3", 4)
        workspace = prepare_workspace(
            {"execution_kind": "non_repo"},
            fence,
            self.profile("local-model"),
            worktree_root=self.worktree_root,
        )
        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-model",
            self.profile("local-model"),
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        from scripts.task_agent_execution import result_manifest

        manifest_id = "publication-" + hashlib.sha256(
            f"{fence.task_id}\0{fence.attempt_id}\0publication".encode()
        ).hexdigest()[:32]
        manifest = result_manifest(prepared, "observed", "", [], manifest_id)
        self.assertEqual(manifest["publication_id"], manifest_id)
        response = self.publication_response(prepared, manifest)
        foreign = self.worktree_root / "unowned-task" / "unowned-attempt"
        foreign.mkdir(parents=True)
        posts: list[str] = []
        gets: list[tuple[str, object]] = []

        def get_json(_base: str, path: str, **kwargs: object) -> dict[str, object]:
            gets.append((path, copy.deepcopy(kwargs.get("params"))))
            return self.restart_publication_response(response)

        def post_json(_base: str, path: str, payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            posts.append(path)
            receipt = copy.deepcopy(payload["cleanup_receipt"])
            receipt.update({"recorded": True, "durable": True, "acknowledged": True})  # type: ignore[union-attr]
            return {"cleanup_receipt": receipt}

        with (
            mock.patch("scripts.task_agent_execution._workspace_processes_absent", return_value={"verified": True, "reason": "no_open_process"}),
            mock.patch("scripts.task_agent_execution._workspace_containers_absent", return_value={"verified": True, "reason": "container_absent", "task_ref": "opaque"}),
        ):
            reconciled = reconcile_owned_workspaces(
                orchestrator_url="http://gateway.invalid",
                worker="worker-u3",
                worker_instance="instance-u3",
                get_json=get_json,
                post_json=post_json,
                worktree_root=self.worktree_root,
            )
        self.assertEqual(reconciled["examined"], 1)
        self.assertEqual(len(reconciled["cleaned"]), 1)
        self.assertEqual(reconciled["retained"], [])
        self.assertFalse(workspace.cwd.exists())
        self.assertTrue(foreign.exists())
        self.assertEqual(posts, [f"/agents/tasks/{fence.task_id}/cleanup"])
        self.assertEqual(
            gets,
            [
                (
                    f"/agents/tasks/{fence.task_id}/attempts/{fence.attempt_id}/publication",
                    {
                        "lease_id": fence.lease_id,
                        "generation": str(fence.generation),
                        "worker_id": fence.worker_id,
                        "worker_instance_id": fence.worker_instance_id,
                        "idempotency_key": f"task-result:{manifest['result_id']}",
                    },
                )
            ],
        )

    def test_shared_core_reconciliation_fixtures_pass_exact_production_consumer(self) -> None:
        fence = LeaseFence(
            "task_fixture",
            "attempt_fixture",
            "lease_fixture",
            "worker-fixture",
            "worker-instance-fixture",
            3,
        )
        expected_states = {
            "agent_task_publication_reconciliation.v1.json": ("writeback_pending", "pending"),
            "agent_task_publication_reconciliation.v1.writeback_failed.json": ("writeback_failed", "failed"),
            "agent_task_publication_reconciliation.v1.committed.json": ("committed", "committed"),
            "agent_task_publication_reconciliation.v1.dead_letter.json": ("dead_letter", "dead_letter"),
        }
        for name in CORE_RECONCILIATION_FIXTURES:
            with self.subTest(name=name):
                fixture = self.core_reconciliation_fixture(name)
                publication, receipt, authorization = validate_publication_reconciliation(
                    fixture,
                    fence=fence,
                    publication_id="publication_fixture",
                    result_id="result_fixture",
                    workspace_ref="workspace-fixture",
                    idempotency_key="task-result:result_fixture",
                )
                self.assertEqual(
                    (publication["status"], publication["writeback_status"]),
                    expected_states[name],
                )
                self.assertEqual(receipt["state"], "staged")
                self.assertTrue(authorization["authorized"])

        pending = self.core_reconciliation_fixture(CORE_RECONCILIATION_FIXTURES[0])
        calls: list[tuple[str, object]] = []

        def get_json(_base: str, path: str, **kwargs: object) -> dict[str, object]:
            calls.append((path, copy.deepcopy(kwargs.get("params"))))
            return copy.deepcopy(pending)

        publication, receipt, _authorization = fetch_attempt_publication_receipt(
            "http://gateway.invalid",
            fence=fence,
            publication_id="publication_fixture",
            result_id="result_fixture",
            workspace_ref="workspace-fixture",
            get_json=get_json,
        )
        self.assertEqual(publication["schema_id"], "agent_task_publication_reconciliation.v1")
        self.assertEqual(receipt["state"], "staged")
        self.assertEqual(
            calls,
            [
                (
                    "/agents/tasks/task_fixture/attempts/attempt_fixture/publication",
                    {
                        "lease_id": "lease_fixture",
                        "generation": "3",
                        "worker_id": "worker-fixture",
                        "worker_instance_id": "worker-instance-fixture",
                        "idempotency_key": "task-result:result_fixture",
                    },
                )
            ],
        )

    def test_reconciliation_contract_rejects_missing_extra_and_crossed_states(self) -> None:
        fixture = self.core_reconciliation_fixture(CORE_RECONCILIATION_FIXTURES[0])
        fence = LeaseFence(
            "task_fixture",
            "attempt_fixture",
            "lease_fixture",
            "worker-fixture",
            "worker-instance-fixture",
            3,
        )
        variants = []
        missing = copy.deepcopy(fixture)
        missing.pop("lease_generation")
        variants.append(missing)
        extra = copy.deepcopy(fixture)
        extra["ok"] = True
        variants.append(extra)
        crossed = copy.deepcopy(fixture)
        crossed["status"] = "committed"
        variants.append(crossed)
        committed_receipt = copy.deepcopy(fixture)
        committed_receipt["publication_receipt"]["state"] = "committed"  # type: ignore[index]
        variants.append(committed_receipt)
        boolean_generation = copy.deepcopy(fixture)
        boolean_generation["generation"] = True
        boolean_generation["assignment_generation"] = True
        boolean_generation["lease_generation"] = True
        variants.append(boolean_generation)
        for variant in variants:
            with self.subTest(keys=sorted(variant)):
                with self.assertRaises(ExecutionBlocked) as blocked:
                    validate_publication_reconciliation(
                        variant,
                        fence=fence,
                        publication_id="publication_fixture",
                        result_id="result_fixture",
                        workspace_ref="workspace-fixture",
                        idempotency_key="task-result:result_fixture",
                    )
                self.assertEqual(blocked.exception.reason, "publication_receipt_invalid")

    def test_reconciliation_lookup_failures_are_static_retryable_reasons(self) -> None:
        fence = LeaseFence("task", "attempt", "lease", "worker", "instance", 1)

        class Response:
            def __init__(self, status_code: int) -> None:
                self.status_code = status_code

        class HTTPFailure(Exception):
            def __init__(self, status_code: int) -> None:
                self.response = Response(status_code)

        TransportFailure = type("TransportError", (Exception,), {"__module__": "httpx"})
        cases = (
            (HTTPFailure(404), "reconciliation_publication_not_found"),
            (HTTPFailure(503), "reconciliation_gateway_unavailable"),
            (TransportFailure("API_KEY=transport-secret /Users/private"), "reconciliation_transport_unavailable"),
            (RuntimeError("ContextLattice request failed status=404: password=fallback-secret"), "reconciliation_publication_not_found"),
            (RuntimeError("ContextLattice request failed status=503: password=fallback-secret /var/private"), "reconciliation_gateway_unavailable"),
        )
        for failure, reason in cases:
            with self.subTest(reason=reason):
                with self.assertRaises(ReconciliationLookupRetryable) as retryable:
                    fetch_attempt_publication_receipt(
                        "http://gateway.invalid",
                        fence=fence,
                        publication_id="publication",
                        result_id="result",
                        workspace_ref="workspace-ref",
                        get_json=lambda *_args, _failure=failure, **_kwargs: (_ for _ in ()).throw(_failure),
                    )
                self.assertEqual(retryable.exception.reason, reason)
                self.assertNotIn("secret", str(retryable.exception))
                self.assertNotIn("/Users", str(retryable.exception))
        with self.assertRaises(RuntimeError):
            fetch_attempt_publication_receipt(
                "http://gateway.invalid",
                fence=fence,
                publication_id="publication",
                result_id="result",
                workspace_ref="workspace-ref",
                get_json=lambda *_args, **_kwargs: (_ for _ in ()).throw(RuntimeError("programmer error")),
            )

    def test_reconciliation_temporary_failure_retains_one_orphan_and_continues(self) -> None:
        records: dict[str, tuple[PreparedExecution, dict[str, object]]] = {}
        for suffix in ("retry", "clean"):
            fence = LeaseFence(
                f"task-{suffix}",
                f"attempt-{suffix}",
                f"lease-{suffix}",
                "worker-u3",
                "instance-u3",
                7,
            )
            workspace = prepare_workspace(
                {"execution_kind": "non_repo"},
                fence,
                self.profile("local-model"),
                worktree_root=self.worktree_root,
            )
            prepared = PreparedExecution(
                {"id": fence.task_id},
                {"attempt_id": fence.attempt_id},
                fence,
                "local-model",
                self.profile("local-model"),
                SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
                workspace,
                None,
                {},
                "",
            )
            manifest = result_manifest(
                prepared,
                "observed",
                "",
                [],
                "publication-" + hashlib.sha256(f"{fence.task_id}\0{fence.attempt_id}\0publication".encode()).hexdigest()[:32],
            )
            records[fence.task_id] = (prepared, self.restart_publication_response(self.publication_response(prepared, manifest)))

        def get_json(_base: str, path: str, **_kwargs: object) -> dict[str, object]:
            if "/task-retry/" in path:
                raise RuntimeError("ContextLattice request failed status=503: API_KEY=temporary-secret /Users/private")
            return copy.deepcopy(records["task-clean"][1])

        def post_json(_base: str, _path: str, payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            receipt = copy.deepcopy(payload["cleanup_receipt"])
            receipt.update({"recorded": True, "durable": True, "acknowledged": True})  # type: ignore[union-attr]
            return {"cleanup_receipt": receipt}

        with (
            mock.patch("scripts.task_agent_execution._workspace_processes_absent", return_value={"verified": True, "reason": "no_open_process"}),
            mock.patch("scripts.task_agent_execution._workspace_containers_absent", return_value={"verified": True, "reason": "container_absent", "task_ref": "opaque"}),
        ):
            reconciled = reconcile_owned_workspaces(
                orchestrator_url="http://gateway.invalid",
                worker="worker-u3",
                worker_instance="instance-u3",
                get_json=get_json,
                post_json=post_json,
                worktree_root=self.worktree_root,
            )
        self.assertEqual(reconciled["examined"], 2)
        self.assertEqual(len(reconciled["cleaned"]), 1)
        self.assertEqual(
            [item["reason"] for item in reconciled["retained"]],
            ["reconciliation_gateway_unavailable"],
        )
        self.assertTrue(records["task-retry"][0].workspace.cwd.exists())
        self.assertFalse(records["task-clean"][0].workspace.cwd.exists())

    def test_reconciliation_does_not_swallow_programmer_errors(self) -> None:
        fence = LeaseFence("task-programmer", "attempt-programmer", "lease", "worker-u3", "instance-u3", 1)
        workspace = prepare_workspace(
            {"execution_kind": "non_repo"},
            fence,
            self.profile("local-model"),
            worktree_root=self.worktree_root,
        )
        with self.assertRaises(TypeError):
            reconcile_owned_workspaces(
                orchestrator_url="http://gateway.invalid",
                worker="worker-u3",
                worker_instance="instance-u3",
                get_json=lambda *_args, **_kwargs: (_ for _ in ()).throw(TypeError("programmer error")),
                post_json=lambda *_args, **_kwargs: self.fail("cleanup must not run"),
                worktree_root=self.worktree_root,
            )
        self.assertTrue(workspace.cwd.exists())

    def test_real_reconciliation_proves_absence_and_cleans_exact_nonrepo_attempt(self) -> None:
        fence = LeaseFence("task-real-orphan", "attempt-real-orphan", "lease", "worker-u3", "instance-u3", 5)
        workspace = prepare_workspace(
            {"execution_kind": "non_repo"},
            fence,
            self.profile("local-model"),
            worktree_root=self.worktree_root,
        )
        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-model",
            self.profile("local-model"),
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        publication_id = "publication-" + hashlib.sha256(
            f"{fence.task_id}\0{fence.attempt_id}\0publication".encode()
        ).hexdigest()[:32]
        manifest = result_manifest(prepared, "observed", "", [], publication_id)
        response = self.publication_response(prepared, manifest)
        gets: list[tuple[str, object]] = []

        def get_json(_base: str, path: str, **kwargs: object) -> dict[str, object]:
            gets.append((path, copy.deepcopy(kwargs.get("params"))))
            return self.restart_publication_response(response)

        def post_json(_base: str, _path: str, payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            recorded = copy.deepcopy(payload["cleanup_receipt"])
            recorded.update({"recorded": True, "durable": True, "acknowledged": True})  # type: ignore[union-attr]
            return {"cleanup_receipt": recorded}

        reconciled = reconcile_owned_workspaces(
            orchestrator_url="http://gateway.invalid",
            worker="worker-u3",
            worker_instance="instance-u3",
            get_json=get_json,
            post_json=post_json,
            worktree_root=self.worktree_root,
        )
        self.assertEqual(reconciled["examined"], 1)
        self.assertEqual(len(reconciled["cleaned"]), 1)
        self.assertEqual(reconciled["retained"], [])
        self.assertFalse(workspace.cwd.exists())
        self.assertEqual(
            gets,
            [
                (
                    f"/agents/tasks/{fence.task_id}/attempts/{fence.attempt_id}/publication",
                    {
                        "lease_id": fence.lease_id,
                        "generation": str(fence.generation),
                        "worker_id": fence.worker_id,
                        "worker_instance_id": fence.worker_instance_id,
                        "idempotency_key": f"task-result:{manifest['result_id']}",
                    },
                )
            ],
        )

    def test_reconciliation_retains_workspace_when_liveness_is_unproven(self) -> None:
        fence = LeaseFence("task-retain", "attempt-retain", "lease", "worker-u3", "instance-u3", 2)
        workspace = prepare_workspace(
            {"execution_kind": "non_repo"},
            fence,
            self.profile("local-model"),
            worktree_root=self.worktree_root,
        )
        prepared = PreparedExecution(
            {},
            {},
            fence,
            "local-model",
            self.profile("local-model"),
            SnapshotBinding("", "", "", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        from scripts.task_agent_execution import result_manifest

        manifest = result_manifest(prepared, "observed", "", [], "publication-" + hashlib.sha256(f"{fence.task_id}\0{fence.attempt_id}\0publication".encode()).hexdigest()[:32])
        response = self.publication_response(prepared, manifest)
        with (
            mock.patch("scripts.task_agent_execution._workspace_processes_absent", return_value={"verified": False, "reason": "workspace_process_present"}),
            mock.patch("scripts.task_agent_execution._workspace_containers_absent", return_value={"verified": True, "reason": "container_absent", "task_ref": "opaque"}),
        ):
            reconciled = reconcile_owned_workspaces(
                orchestrator_url="http://gateway.invalid",
                worker="worker-u3",
                worker_instance="instance-u3",
                get_json=lambda *_args, **_kwargs: self.restart_publication_response(response),
                post_json=lambda *_args, **_kwargs: self.fail("cleanup receipt must not be reported"),
                worktree_root=self.worktree_root,
            )
        self.assertEqual(len(reconciled["retained"]), 1)
        self.assertEqual(reconciled["retained"][0]["reason"], "reconciliation_liveness_unverified")
        self.assertTrue(workspace.cwd.exists())

    def test_runtime_config_failure_is_retryable_and_blocks_reconciliation_cleaned(self) -> None:
        task_ref = "runtime-proof-u3"
        runtime_root = self.worktree_root / ".runtime"
        runtime_root.mkdir(mode=0o700)
        candidate = runtime_root / f"contextlattice-task-{task_ref}-owned"
        candidate.mkdir(mode=0o700)
        (candidate / "config.json").write_text('{"auths":{}}\n', encoding="utf-8")
        (candidate / "container.cid").write_text("a" * 64, encoding="ascii")
        with mock.patch("scripts.task_agent_execution.shutil.rmtree", return_value=None):
            failed = task_execution._remove_owned_runtime_dirs(self.worktree_root, task_ref)
        self.assertFalse(failed["verified"])
        self.assertTrue(candidate.exists())
        retried = task_execution._remove_owned_runtime_dirs(self.worktree_root, task_ref)
        self.assertTrue(retried["verified"])
        self.assertFalse(candidate.exists())

        fence = LeaseFence("task-runtime-reconcile", "attempt-runtime-reconcile", "lease", "worker-u3", "instance-u3", 1)
        workspace = prepare_workspace(
            {"execution_kind": "non_repo"},
            fence,
            self.profile("local-model"),
            worktree_root=self.worktree_root,
        )
        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-model",
            self.profile("local-model"),
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        manifest = result_manifest(prepared, "observed", "", [], task_execution._publication_id(fence))
        response = self.publication_response(prepared, manifest)

        def post_json(_base: str, _path: str, _payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            self.fail("cleanup must not be reported when runtime proof fails")

        with (
            mock.patch("scripts.task_agent_execution._workspace_processes_absent", return_value={"verified": True, "reason": "workspace_absent"}),
            mock.patch("scripts.task_agent_execution._workspace_containers_absent", return_value={"verified": False, "reason": "runtime_config_cleanup_failed"}),
        ):
            reconciled = reconcile_owned_workspaces(
                orchestrator_url="http://gateway.invalid",
                worker="worker-u3",
                worker_instance="instance-u3",
                get_json=lambda *_args, **_kwargs: self.restart_publication_response(response),
                post_json=post_json,
                worktree_root=self.worktree_root,
            )
        self.assertEqual(reconciled["cleaned"], [])
        self.assertEqual(reconciled["retained"][0]["reason"], "reconciliation_liveness_unverified")
        self.assertTrue(workspace.cwd.exists())
        self.assertEqual(reconciled["retained"][0]["evidence"]["container"]["reason"], "runtime_config_cleanup_failed")

    def test_runner_result_protocol_rejects_malformed_schema_and_foreign_binding(self) -> None:
        prepared = self.prepared_coding()
        valid = validate_runner_result(prepared, self.result_capture(self.runner_envelope(prepared)))
        self.assertEqual(valid["status"], "succeeded")
        self.assertTrue(valid["digest"].startswith("sha256:"))

        malformed = CaptureResult(0, b"not-json", b"", False, False, False, "execution_observed", {"verified": True})
        with self.assertRaises(ExecutionBlocked) as malformed_result:
            validate_runner_result(prepared, malformed)
        self.assertEqual(malformed_result.exception.reason, "runner_result_invalid")

        wrong_schema = self.runner_envelope(prepared)
        wrong_schema["schema_id"] = "foreign.v1"
        with self.assertRaises(ExecutionBlocked) as schema_result:
            validate_runner_result(prepared, self.result_capture(wrong_schema))
        self.assertEqual(schema_result.exception.reason, "runner_result_schema_mismatch")

        foreign = self.runner_envelope(prepared)
        foreign["task_id"] = "foreign-task"
        with self.assertRaises(ExecutionBlocked) as foreign_result:
            validate_runner_result(prepared, self.result_capture(foreign))
        self.assertEqual(foreign_result.exception.reason, "runner_result_binding_mismatch")

    def test_inference_result_requires_nonempty_public_closed_output(self) -> None:
        invalid = (
            ("", {}),
            ("   ", {"provider": "local"}),
            ("valid output", {"unexpected": "arbitrary"}),
            ("valid output", {"coreml_enabled": "true"}),
            ("password=U3-INFERENCE-SECRET", {"provider": "local"}),
        )
        for output, metadata in invalid:
            with self.subTest(output=output, metadata=metadata):
                with self.assertRaises(ExecutionBlocked) as blocked:
                    validate_inference_result(output, metadata)
                self.assertEqual(blocked.exception.reason, "inference_result_invalid")
        output, metadata = validate_inference_result(
            "bounded public inference",
            {"provider": "local", "transport": "gateway", "coreml_enabled": False},
        )
        self.assertEqual(output, "bounded public inference")
        self.assertEqual(metadata["provider"], "local")

    def test_prepared_none_argv_cannot_publish_empty_or_arbitrary_inference(self) -> None:
        fence = LeaseFence("task-inference-empty", "attempt-inference-empty", "lease", "worker", "instance", 1)
        profile = self.profile("local-model")
        workspace = prepare_workspace({"execution_kind": "non_repo"}, fence, profile, worktree_root=self.worktree_root)
        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-model",
            profile,
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        posts: list[str] = []

        def post_json(_base: str, path: str, _payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            posts.append(path)
            return {"ok": True}

        with self.assertRaises(ExecutionBlocked) as blocked:
            execute_prepared(
                prepared,
                orchestrator_url="http://gateway.invalid",
                post_json=post_json,
                gateway_inference=lambda _prepared, _lost: ("", {"unexpected": "arbitrary"}),
            )
        self.assertEqual(blocked.exception.reason, "inference_result_invalid")
        self.assertFalse(any(path.endswith("/publish") for path in posts))

    def test_authoritative_heartbeat_loss_cancels_blocked_gateway_inference_before_publish(self) -> None:
        fence = LeaseFence("task-inference-cancel", "attempt-inference-cancel", "lease", "worker", "instance", 1)
        profile = self.profile("local-model")
        profile["heartbeat_interval_secs"] = 1.0
        workspace = prepare_workspace(
            {"execution_kind": "non_repo"},
            fence,
            profile,
            worktree_root=self.worktree_root,
        )
        prepared = PreparedExecution(
            {"id": fence.task_id},
            {"attempt_id": fence.attempt_id},
            fence,
            "local-model",
            profile,
            SnapshotBinding("snapshot", "hash", "session", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        posts: list[str] = []
        callback_started = threading.Event()
        callback_stopped = threading.Event()

        def post_json(_base: str, path: str, _payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            posts.append(path)
            if path.endswith("/heartbeat"):
                return {"ok": False, "reason": "lease_revoked"}
            if path.endswith("/publish"):
                self.fail("lease-lost inference must never publish")
            return {"ok": True, "status": "running"}

        def gateway_inference(_prepared: PreparedExecution, lost: threading.Event) -> tuple[str, dict[str, object]]:
            callback_started.set()
            if not lost.wait(2.5):
                self.fail("authoritative heartbeat did not cancel blocked inference")
            callback_stopped.set()
            raise ExecutionBlocked(
                "lease_lost",
                "authoritative lease loss canceled blocked inference",
                execution_observed=True,
            )

        started = time.monotonic()
        with self.assertRaises(ExecutionBlocked) as blocked:
            execute_prepared(
                prepared,
                orchestrator_url="http://gateway.invalid",
                post_json=post_json,
                gateway_inference=gateway_inference,
            )
        elapsed = time.monotonic() - started
        self.assertEqual(blocked.exception.reason, "lease_lost")
        self.assertTrue(callback_started.is_set())
        self.assertTrue(callback_stopped.is_set())
        self.assertTrue(any(path.endswith("/heartbeat") for path in posts))
        self.assertFalse(any(path.endswith("/publish") for path in posts))
        self.assertLess(elapsed, 2.0)

    def test_exit_zero_with_invalid_runner_outputs_is_execution_failed(self) -> None:
        from scripts.task_agent_execution import prepare_execution

        cases = (
            ("malformed", "runner_result_invalid"),
            ("wrong-schema", "runner_result_schema_mismatch"),
            ("foreign-task", "runner_result_binding_mismatch"),
        )
        for case, expected_reason in cases:
            with self.subTest(case=case):
                repo, base = self.git_repo()
                claim = self.claim(profile="local-coding", kind="coding", repo=repo, base_sha=base)
                case_root = self.new_root(f"runner-output-{case}")
                prepared = prepare_execution(
                    claim,
                    worker="worker-u3",
                    orchestrator_url="http://gateway.invalid",
                    get_json=self.snapshot_getter(claim),
                    source_repo=repo,
                    worktree_root=case_root,
                )
                if case == "malformed":
                    output = "not-json"
                else:
                    envelope = self.runner_envelope(prepared)
                    if case == "wrong-schema":
                        envelope["schema_id"] = "foreign.v1"
                    else:
                        envelope["task_id"] = "foreign-task"
                    output = json.dumps(envelope, separators=(",", ":"))
                script = prepared.workspace.cwd / "invalid-output.sh"
                script.write_text(f"#!/bin/sh\nset -eu\nprintf '%s\\n' '{output}'\n", encoding="utf-8")
                script.chmod(0o755)
                prepared = replace(prepared, argv=["/workspace/invalid-output.sh"])
                posts: list[str] = []

                def post_json(_base: str, path: str, _payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
                    posts.append(path)
                    return {"ok": True, "status": "running"}

                result = execute_prepared(prepared, orchestrator_url="http://gateway.invalid", post_json=post_json)

                self.assertEqual(result["status"], "execution_failed", result)
                self.assertEqual(result["metadata"]["result_blocked"], expected_reason)
                self.assertFalse(any(path.endswith("/publish") for path in posts))

    def test_initial_identity_failures_preserve_unverified_cleanup_as_quarantine(self) -> None:
        original_root = self.worktree_root
        try:
            for case in ("identity", "group"):
                with self.subTest(case=case):
                    self.worktree_root = self.new_root(f"initial-{case}-worktrees")
                    os.environ["CONTEXTLATTICE_TASK_WORKTREE_ROOT"] = str(self.worktree_root)
                    prepared = self.prepared_coding()
                    config_dir = self.new_root(f"initial-{case}-config")
                    boundary = ContainerBoundary(
                        ["/bin/sleep", "5"],
                        {"PATH": "/usr/bin:/bin"},
                        Path("/bin/true"),
                        f"fake-{case}",
                        config_dir,
                        config_dir / "container.cid",
                        "ab" * 16,
                        f"task-ref-{case}",
                        5.0,
                    )
                    fake_proc = mock.Mock()
                    fake_proc.pid = 424220 if case == "identity" else 424221
                    fake_proc.stdout = io.BytesIO()
                    fake_proc.stderr = io.BytesIO()
                    fake_proc.poll.return_value = None
                    fake_proc.wait.side_effect = subprocess.TimeoutExpired("fake-boundary", 1)

                    def description(pid: int) -> dict[str, object] | None:
                        if case == "identity":
                            return None
                        return {
                            "pid": pid,
                            "ppid": os.getpid(),
                            "pgid": pid + 1,
                            "command": "/bin/sleep 5",
                            "cwd": str(prepared.workspace.cwd),
                        }

                    with (
                        mock.patch("scripts.task_agent_execution._container_boundary", return_value=boundary),
                        mock.patch("scripts.task_agent_execution.subprocess.Popen", return_value=fake_proc),
                        mock.patch("scripts.task_agent_execution._process_description", side_effect=description),
                        mock.patch(
                            "scripts.task_agent_execution._remove_container",
                            return_value={"verified": False, "quarantined": True, "reason": "container_liveness_unverified"},
                        ),
                    ):
                        with self.assertRaises(ExecutionBlocked) as blocked:
                            run_bounded_process(
                                ["/workspace/fixture"],
                                prepared.env,
                                prepared.workspace.cwd,
                                5,
                                profile=prepared.profile,
                            )
                    self.assertEqual(blocked.exception.reason, "quarantined")
                    self.assertTrue(blocked.exception.execution_observed)
                    self.assertTrue(prepared.workspace.cwd.exists())
                    self.assertTrue(
                        (self.worktree_root / prepared.fence.task_id / f"{prepared.fence.attempt_id}.owner.json").exists()
                    )
                    self.assertFalse(blocked.exception.evidence["container"]["verified"])
                    fake_proc.kill.assert_not_called()
        finally:
            self.worktree_root = original_root
            os.environ["CONTEXTLATTICE_TASK_WORKTREE_ROOT"] = str(original_root)

    def test_initial_attestation_cleanup_runs_when_kill_or_wait_raises(self) -> None:
        original_root = self.worktree_root
        try:
            for failing_call in ("kill", "wait", "stdout_close"):
                with self.subTest(failing_call=failing_call):
                    self.worktree_root = self.new_root(f"initial-{failing_call}-finally-worktrees")
                    os.environ["CONTEXTLATTICE_TASK_WORKTREE_ROOT"] = str(self.worktree_root)
                    prepared = self.prepared_coding()
                    config_dir = self.new_root(f"fake-finally-{failing_call}-config")
                    cidfile = config_dir / "container.cid"
                    cidfile.write_text("a" * 64, encoding="ascii")
                    run_nonce = "ab" * 16
                    boundary = ContainerBoundary(
                        [
                            "/bin/false",
                            "run",
                            f"--label=io.contextlattice.run-nonce={run_nonce}",
                            "/workspace/fixture",
                        ],
                        {"PATH": "/usr/bin:/bin"},
                        Path("/bin/true"),
                        f"fake-finally-{failing_call}",
                        config_dir,
                        cidfile,
                        run_nonce,
                        f"task-ref-finally-{failing_call}",
                        5.0,
                    )
                    fake_proc = mock.Mock()
                    fake_proc.pid = 424242
                    fake_proc.stdout = io.BytesIO()
                    fake_proc.stderr = io.BytesIO()
                    if failing_call == "stdout_close":
                        fake_proc.stdout = mock.Mock()
                        fake_proc.stdout.close.side_effect = RuntimeError("close failed")
                    fake_proc.poll.side_effect = [None, None, 0]
                    fake_proc.kill.side_effect = OSError("kill failed") if failing_call == "kill" else None
                    fake_proc.wait.side_effect = OSError("wait failed") if failing_call == "wait" else None
                    remove_container = mock.Mock(
                        return_value={
                            "verified": True,
                            "quarantined": False,
                            "reason": "container_absent",
                            "container_ref": "container-proof",
                        }
                    )
                    owned_description = {
                        "pid": fake_proc.pid,
                        "ppid": os.getpid(),
                        "pgid": fake_proc.pid,
                        "command": " ".join(boundary.argv),
                        "cwd": str(prepared.workspace.cwd),
                    }
                    with (
                        mock.patch("scripts.task_agent_execution._container_boundary", return_value=boundary),
                        mock.patch("scripts.task_agent_execution.subprocess.Popen", return_value=fake_proc),
                        mock.patch(
                            "scripts.task_agent_execution._process_description",
                            side_effect=(None, owned_description),
                        ),
                        mock.patch(
                            "scripts.task_agent_execution._group_members",
                            return_value=[{"pid": fake_proc.pid}],
                        ),
                        mock.patch("scripts.task_agent_execution._remove_container", remove_container),
                    ):
                        with self.assertRaises(ExecutionBlocked) as blocked:
                            run_bounded_process(
                                ["/workspace/fixture"],
                                prepared.env,
                                prepared.workspace.cwd,
                                5,
                                profile=prepared.profile,
                            )
                    self.assertEqual(blocked.exception.reason, "quarantined")
                    self.assertTrue(blocked.exception.execution_observed)
                    self.assertFalse(blocked.exception.evidence["process"]["verified"])
                    self.assertTrue(blocked.exception.evidence["container"]["verified"])
                    self.assertTrue(blocked.exception.evidence["config"]["verified"])
                    self.assertFalse(config_dir.exists())
                    self.assertFalse(cidfile.exists())
                    fake_proc.kill.assert_called_once_with()
                    remove_container.assert_called_once_with(boundary)
        finally:
            self.worktree_root = original_root
            os.environ["CONTEXTLATTICE_TASK_WORKTREE_ROOT"] = str(original_root)

    def test_stream_read_failure_is_sanitized_and_cannot_report_success(self) -> None:
        prepared = self.prepared_coding()

        class FailingRead:
            def read(self, _size: int) -> bytes:
                raise RuntimeError("password=thread-secret /opt/private")

            def close(self) -> None:
                return None

        boundary, fake_proc, description = self.finished_process_boundary(
            prepared,
            "stream-read-failure",
            stdout=FailingRead(),
        )
        remove_container = mock.Mock(
            return_value={"verified": True, "quarantined": False, "reason": "container_absent"}
        )
        with (
            mock.patch("scripts.task_agent_execution._container_boundary", return_value=boundary),
            mock.patch("scripts.task_agent_execution.subprocess.Popen", return_value=fake_proc),
            mock.patch("scripts.task_agent_execution._process_description", return_value=description),
            mock.patch(
                "scripts.task_agent_execution.terminate_owned_process_group",
                return_value={"verified": True, "quarantined": False, "reason": "already_exited"},
            ),
            mock.patch("scripts.task_agent_execution._remove_container", remove_container),
        ):
            capture = run_bounded_process(
                ["/workspace/fixture"],
                prepared.env,
                prepared.workspace.cwd,
                5,
                profile=prepared.profile,
            )
        self.assertEqual(capture.outcome, "execution_failed")
        self.assertNotEqual(capture.returncode, 0)
        self.assertEqual(capture.termination["stream_failures"], ["stdout"])
        rendered = json.dumps(capture.termination, sort_keys=True)
        self.assertNotIn("thread-secret", rendered)
        self.assertNotIn("/opt/private", rendered)
        self.assertFalse(boundary.config_dir.exists())
        remove_container.assert_called_once_with(boundary)

    def test_normal_wait_and_stream_close_failures_still_cleanup_exact_boundary(self) -> None:
        prepared = self.prepared_coding()

        class CloseFails:
            def read(self, _size: int) -> bytes:
                return b""

            def close(self) -> None:
                raise RuntimeError("password=close-secret /var/private")

        for failure in ("wait", "stdout_close"):
            with self.subTest(failure=failure):
                stdout: object = CloseFails() if failure == "stdout_close" else io.BytesIO()
                boundary, fake_proc, description = self.finished_process_boundary(
                    prepared,
                    f"normal-cleanup-{failure}",
                    stdout=stdout,
                )
                if failure == "wait":
                    fake_proc.wait.side_effect = RuntimeError("password=wait-secret /opt/private")
                remove_container = mock.Mock(
                    return_value={"verified": True, "quarantined": False, "reason": "container_absent"}
                )
                with (
                    mock.patch("scripts.task_agent_execution._container_boundary", return_value=boundary),
                    mock.patch("scripts.task_agent_execution.subprocess.Popen", return_value=fake_proc),
                    mock.patch("scripts.task_agent_execution._process_description", return_value=description),
                    mock.patch(
                        "scripts.task_agent_execution.terminate_owned_process_group",
                        return_value={"verified": True, "quarantined": False, "reason": "already_exited"},
                    ),
                    mock.patch("scripts.task_agent_execution._remove_container", remove_container),
                ):
                    try:
                        capture = run_bounded_process(
                            ["/workspace/fixture"],
                            prepared.env,
                            prepared.workspace.cwd,
                            5,
                            profile=prepared.profile,
                        )
                    except RuntimeError as exc:
                        self.fail(f"normal cleanup exception escaped: {type(exc).__name__}")
                self.assertEqual(capture.outcome, "execution_failed")
                self.assertNotEqual(capture.returncode, 0)
                self.assertIn(failure, capture.termination["cleanup_failures"])
                self.assertFalse(boundary.config_dir.exists())
                remove_container.assert_called_once_with(boundary)

    def test_runtime_config_removal_requires_exact_absence_proof(self) -> None:
        prepared = self.prepared_coding()
        boundary, fake_proc, description = self.finished_process_boundary(prepared, "config-removal-proof")
        real_rmtree = shutil.rmtree

        def retain_config(target: object, *args: object, **kwargs: object) -> None:
            if kwargs.get("dir_fd") is not None and Path(target).name == boundary.config_dir.name:
                return None
            real_rmtree(target, *args, **kwargs)  # type: ignore[arg-type]

        try:
            with (
                mock.patch("scripts.task_agent_execution._container_boundary", return_value=boundary),
                mock.patch("scripts.task_agent_execution.subprocess.Popen", return_value=fake_proc),
                mock.patch("scripts.task_agent_execution._process_description", return_value=description),
                mock.patch(
                    "scripts.task_agent_execution.terminate_owned_process_group",
                    return_value={"verified": True, "quarantined": False, "reason": "already_exited"},
                ),
                mock.patch(
                    "scripts.task_agent_execution._remove_container",
                    return_value={"verified": True, "quarantined": False, "reason": "container_absent"},
                ),
                mock.patch("scripts.task_agent_execution.shutil.rmtree", side_effect=retain_config),
            ):
                capture = run_bounded_process(
                    ["/workspace/fixture"],
                    prepared.env,
                    prepared.workspace.cwd,
                    5,
                    profile=prepared.profile,
                )
            self.assertEqual(capture.outcome, "quarantined")
            self.assertFalse(capture.termination["config"]["verified"])
            self.assertTrue(boundary.config_dir.exists())
            self.assertTrue(boundary.cidfile.exists())
        finally:
            real_rmtree(boundary.config_dir, ignore_errors=True)

    def test_exact_cleanup_uses_open_parent_dirfd_across_parent_swap(self) -> None:
        parent = self.new_root("fd-parent")
        moved_parent = self.temp_dir / "fd-parent-moved"
        outside = self.new_root("fd-outside")
        target = parent / "owned-config"
        target.mkdir(mode=0o700)
        (target / "secret.txt").write_text("owned", encoding="utf-8")
        outside_target = outside / target.name
        outside_target.mkdir(mode=0o700)
        (outside_target / "secret.txt").write_text("foreign", encoding="utf-8")
        real_rmtree = shutil.rmtree

        def swap_parent_then_remove(name: object, *args: object, **kwargs: object) -> None:
            parent.rename(moved_parent)
            parent.symlink_to(outside, target_is_directory=True)
            real_rmtree(name, *args, **kwargs)  # type: ignore[arg-type]

        try:
            with mock.patch("scripts.task_agent_execution.shutil.rmtree", side_effect=swap_parent_then_remove):
                result = task_execution._remove_exact_owned_directory(
                    target,
                    parent,
                    name_prefix="owned-",
                    reason="fd_parent_swap",
                )
            self.assertTrue(result["verified"])
            self.assertTrue(outside_target.exists())
            self.assertEqual((outside_target / "secret.txt").read_text(encoding="utf-8"), "foreign")
            self.assertFalse((moved_parent / target.name).exists())
        finally:
            if parent.is_symlink():
                parent.unlink()
            real_rmtree(moved_parent, ignore_errors=True)

    def test_fast_exit_without_process_attestation_is_quarantined(self) -> None:
        prepared = self.prepared_coding()
        config_dir = self.new_root("fast-exit-config")
        boundary = ContainerBoundary(
            ["/bin/true"],
            {"PATH": "/usr/bin:/bin"},
            Path("/bin/true"),
            "fast-exit",
            config_dir,
            config_dir / "container.cid",
            "ab" * 16,
            "task-ref-fast-exit",
            5.0,
        )
        fake_proc = mock.Mock()
        fake_proc.pid = 424240
        fake_proc.stdout = io.BytesIO()
        fake_proc.stderr = io.BytesIO()
        poll_results = iter((None, 0))
        fake_proc.poll.side_effect = lambda: next(poll_results, 0)
        fake_proc.wait.return_value = 0
        with (
            mock.patch("scripts.task_agent_execution._container_boundary", return_value=boundary),
            mock.patch("scripts.task_agent_execution.subprocess.Popen", return_value=fake_proc),
            mock.patch("scripts.task_agent_execution._process_description", return_value=None),
            mock.patch("scripts.task_agent_execution._group_members", return_value=[]),
            mock.patch(
                "scripts.task_agent_execution.terminate_owned_process_group",
                return_value={"verified": True, "quarantined": False, "reason": "already_exited"},
            ),
            mock.patch(
                "scripts.task_agent_execution._remove_container",
                return_value={"verified": True, "quarantined": False, "reason": "container_absent"},
            ),
        ):
            with self.assertRaises(ExecutionBlocked) as blocked:
                run_bounded_process(
                    ["/workspace/fixture"],
                    prepared.env,
                    prepared.workspace.cwd,
                    5,
                    profile=prepared.profile,
                )
        self.assertEqual(blocked.exception.reason, "quarantined")
        self.assertTrue(blocked.exception.execution_observed)
        self.assertFalse(config_dir.exists())

    def test_initial_process_attestation_rejects_foreign_run_nonce(self) -> None:
        prepared = self.prepared_coding()
        config_dir = self.new_root("foreign-nonce-config")
        (config_dir / "config.json").write_text('{"auths":{}}\n', encoding="utf-8")
        boundary = ContainerBoundary(
            ["/bin/true", "run", "--label=io.contextlattice.run-nonce=owned-nonce", "/workspace/fixture"],
            {"PATH": "/usr/bin:/bin"},
            Path("/bin/true"),
            "foreign-nonce",
            config_dir,
            config_dir / "container.cid",
            "owned-nonce",
            "task-ref-foreign-nonce",
            5.0,
        )
        fake_proc = mock.Mock()
        fake_proc.pid = 424239
        fake_proc.stdout = io.BytesIO()
        fake_proc.stderr = io.BytesIO()
        fake_proc.poll.return_value = 0
        fake_proc.wait.return_value = 0
        foreign_description = {
            "pid": fake_proc.pid,
            "ppid": os.getpid(),
            "pgid": fake_proc.pid,
            "command": "/bin/true run --label=io.contextlattice.run-nonce=foreign-nonce /workspace/fixture",
            "cwd": str(prepared.workspace.cwd),
        }
        remove_container = mock.Mock(
            return_value={"verified": True, "quarantined": False, "reason": "container_absent"}
        )
        with (
            mock.patch("scripts.task_agent_execution._container_boundary", return_value=boundary),
            mock.patch("scripts.task_agent_execution.subprocess.Popen", return_value=fake_proc),
            mock.patch("scripts.task_agent_execution._process_description", return_value=foreign_description),
            mock.patch(
                "scripts.task_agent_execution.terminate_owned_process_group",
                return_value={"verified": True, "quarantined": False, "reason": "already_exited"},
            ),
            mock.patch("scripts.task_agent_execution._remove_container", remove_container),
        ):
            with self.assertRaises(ExecutionBlocked) as blocked:
                run_bounded_process(
                    ["/workspace/fixture"],
                    prepared.env,
                    prepared.workspace.cwd,
                    5,
                    profile=prepared.profile,
                )
        self.assertEqual(blocked.exception.reason, "quarantined")
        self.assertFalse(blocked.exception.evidence["process"]["verified"])
        self.assertTrue(blocked.exception.evidence["container"]["verified"])
        self.assertFalse(config_dir.exists())
        fake_proc.kill.assert_not_called()
        remove_container.assert_called_once_with(boundary)

    def test_process_identity_requires_exact_executable_and_argv_tokens(self) -> None:
        cwd = self.temp_dir
        nonce = "nonce-u3"
        exact = {
            "pid": 100,
            "ppid": os.getpid(),
            "pgid": 100,
            "command": "/bin/true run --label=io.contextlattice.run-nonce=nonce-u3 /workspace/fixture",
            "argv": ["/bin/true", "run", "--label=io.contextlattice.run-nonce=nonce-u3", "/workspace/fixture"],
            "executable": "/bin/true",
            "executable_inode": None,
            "cwd": str(cwd),
            "_probe_status": "present",
        }
        self.assertTrue(
            task_execution._description_identity_matches(
                exact,
                "/bin/true",
                cwd,
                parent_pids={os.getpid()},
                expected_child_executable="/workspace/fixture",
                expected_run_nonce=nonce,
            )
        )
        spoofed = dict(exact)
        spoofed["command"] = "/tmp/true-spoof run --label=io.contextlattice.run-nonce=nonce-u3 /workspace/fixture"
        spoofed["argv"] = ["/tmp/true-spoof", "run", "--label=io.contextlattice.run-nonce=nonce-u3", "/workspace/fixture"]
        spoofed["executable"] = "/tmp/true-spoof"
        self.assertFalse(
            task_execution._description_identity_matches(
                spoofed,
                "/bin/true",
                cwd,
                parent_pids={os.getpid()},
                expected_child_executable="/workspace/fixture",
                expected_run_nonce=nonce,
            )
        )
        substring_nonce = dict(exact)
        substring_nonce["argv"] = ["/bin/true", "run", "--label=io.contextlattice.run-nonce=nonce-u3-foreign", "/workspace/fixture"]
        self.assertFalse(
            task_execution._description_identity_matches(
                substring_nonce,
                "/bin/true",
                cwd,
                parent_pids={os.getpid()},
                expected_child_executable="/workspace/fixture",
                expected_run_nonce=nonce,
            )
        )

    def test_process_probe_unavailable_or_poll_only_exit_never_verifies_termination(self) -> None:
        fake_proc = mock.Mock()
        fake_proc.pid = 424245
        fake_proc.poll.return_value = 0
        fake_proc.wait.return_value = 0
        with (
            mock.patch("scripts.task_agent_execution._reap_and_describe_direct_leader", return_value=(None, True)),
            mock.patch("scripts.task_agent_execution._group_members", return_value=None),
            mock.patch("scripts.task_agent_execution.os.killpg") as killpg,
        ):
            unavailable = terminate_owned_process_group(
                fake_proc,
                expected_executable="/bin/true",
                expected_cwd=self.temp_dir,
                term_timeout=0.01,
                known_pgid=fake_proc.pid,
            )
        self.assertFalse(unavailable["verified"])
        self.assertEqual(unavailable["reason"], "process_group_probe_unavailable")
        killpg.assert_not_called()

        with (
            mock.patch("scripts.task_agent_execution._reap_and_describe_direct_leader", return_value=({"pid": fake_proc.pid, "_probe_status": "absent"}, True)),
            mock.patch("scripts.task_agent_execution._group_members", return_value=[]),
            mock.patch("scripts.task_agent_execution.os.killpg") as killpg,
        ):
            poll_only = terminate_owned_process_group(
                fake_proc,
                expected_executable="/bin/true",
                expected_cwd=self.temp_dir,
                term_timeout=0.1,
                known_pgid=fake_proc.pid,
            )
        self.assertFalse(poll_only["verified"])
        self.assertEqual(poll_only["reason"], "process_identity_unavailable")
        killpg.assert_not_called()

    def test_ps_and_lsof_permission_errors_are_unknown_not_absence(self) -> None:
        ps_error = subprocess.CompletedProcess(
            ["ps"],
            1,
            "",
            "ps: permission denied",
        )
        with mock.patch("scripts.task_agent_execution.subprocess.run", return_value=ps_error):
            description = task_execution._process_description(424248)
        self.assertEqual(description["_probe_status"], "unavailable")  # type: ignore[index]
        self.assertEqual(description["_probe_evidence"]["stderr"], "ps: permission denied")  # type: ignore[index]

        ps_present = subprocess.CompletedProcess(
            ["ps"],
            0,
            "424248 424200 424248 /bin/true --label=io.contextlattice.run-nonce=u3\n",
            "",
        )
        lsof_error = subprocess.CompletedProcess(
            ["lsof"],
            1,
            "",
            "lsof: permission denied",
        )
        with (
            mock.patch("scripts.task_agent_execution.Path.is_file", return_value=True),
            mock.patch("scripts.task_agent_execution.subprocess.run", side_effect=(ps_present, lsof_error)),
        ):
            description = task_execution._process_description(424248)
        self.assertEqual(description["_probe_status"], "unavailable")  # type: ignore[index]
        self.assertEqual(description["_probe_evidence"]["stderr"], "lsof: permission denied")  # type: ignore[index]

        workspace = self.new_root("lsof-probe-error")
        lsof_error = subprocess.CompletedProcess(
            ["lsof"],
            1,
            "",
            "lsof: permission denied",
        )
        with mock.patch("scripts.task_agent_execution.subprocess.run", return_value=lsof_error):
            result = task_execution._workspace_processes_absent(workspace)
        self.assertFalse(result["verified"])
        self.assertEqual(result["reason"], "process_probe_unavailable")
        self.assertEqual(result["probe_evidence"]["stderr"], "lsof: permission denied")

    def test_nonempty_stderr_with_zero_exit_is_not_probe_success(self) -> None:
        ps_ambiguous = subprocess.CompletedProcess(
            ["ps"],
            0,
            "424248 424200 424248 /bin/true --label=io.contextlattice.run-nonce=u3\n",
            "ps: permission denied at /Users/private/token.txt\n",
        )
        with mock.patch("scripts.task_agent_execution.subprocess.run", return_value=ps_ambiguous):
            description = task_execution._process_description(424248)
        self.assertEqual(description["_probe_status"], "unavailable")  # type: ignore[index]
        self.assertEqual(description["_probe_evidence"]["returncode"], 0)  # type: ignore[index]
        self.assertIn("[LOCAL_PATH]", description["_probe_evidence"]["stderr"])  # type: ignore[index]
        self.assertLessEqual(
            len(description["_probe_evidence"]["stderr"].encode("utf-8")),  # type: ignore[index]
            task_execution.MAX_PROCESS_PROBE_EVIDENCE_BYTES,
        )

        lsof_executable_ambiguous = subprocess.CompletedProcess(
            ["lsof"],
            0,
            "n/bin/true\n",
            "lsof: permission denied\n",
        )
        with (
            mock.patch("scripts.task_agent_execution.Path.is_file", return_value=True),
            mock.patch(
                "scripts.task_agent_execution.subprocess.run",
                side_effect=(
                    subprocess.CompletedProcess(
                        ["ps"],
                        0,
                        "424248 424200 424248 /bin/true\n",
                        "",
                    ),
                    lsof_executable_ambiguous,
                ),
            ),
        ):
            description = task_execution._process_description(424248)
        self.assertEqual(description["_probe_status"], "unavailable")  # type: ignore[index]
        self.assertEqual(description["_probe_evidence"]["tool"], "lsof")  # type: ignore[index]
        self.assertEqual(description["_probe_evidence"]["returncode"], 0)  # type: ignore[index]

        lsof_cwd_ambiguous = subprocess.CompletedProcess(
            ["lsof"],
            0,
            "n/var/task\n",
            "lsof: permission denied\n",
        )
        with (
            mock.patch("scripts.task_agent_execution.Path.is_file", return_value=True),
            mock.patch(
                "scripts.task_agent_execution.subprocess.run",
                side_effect=(
                    subprocess.CompletedProcess(
                        ["ps"],
                        0,
                        "424248 424200 424248 /bin/true\n",
                        "",
                    ),
                    subprocess.CompletedProcess(["lsof"], 0, "n/usr/bin/true\n", ""),
                    lsof_cwd_ambiguous,
                ),
            ),
        ):
            description = task_execution._process_description(424248)
        self.assertEqual(description["_probe_status"], "unavailable")  # type: ignore[index]
        self.assertEqual(description["_probe_evidence"]["tool"], "lsof")  # type: ignore[index]
        self.assertEqual(description["_probe_evidence"]["returncode"], 0)  # type: ignore[index]

        group_ambiguous = subprocess.CompletedProcess(
            ["ps"],
            0,
            "424248 424200 424248 /bin/true\n",
            "ps: permission denied\n",
        )
        with mock.patch("scripts.task_agent_execution.subprocess.run", return_value=group_ambiguous):
            members = task_execution._group_members(424248)
        self.assertIsInstance(members, task_execution._ProbeUnavailable)
        self.assertEqual(members.evidence["returncode"], 0)  # type: ignore[union-attr]
        self.assertEqual(members.evidence["stderr"].strip(), "ps: permission denied")  # type: ignore[union-attr]

        descendants_ambiguous = subprocess.CompletedProcess(
            ["ps"],
            0,
            "424249 424248\n",
            "ps: permission denied\n",
        )
        with mock.patch("scripts.task_agent_execution.subprocess.run", return_value=descendants_ambiguous):
            descendants = task_execution._descendants(424248)
        self.assertIsInstance(descendants, task_execution._ProbeUnavailable)
        self.assertEqual(descendants.evidence["returncode"], 0)  # type: ignore[union-attr]
        self.assertEqual(descendants.evidence["stderr"].strip(), "ps: permission denied")  # type: ignore[union-attr]

        workspace = self.new_root("lsof-zero-exit-stderr")
        lsof_ambiguous = subprocess.CompletedProcess(
            ["lsof"],
            0,
            "COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME\n",
            "lsof: permission denied at /Users/private/token.txt\n",
        )
        with mock.patch("scripts.task_agent_execution.subprocess.run", return_value=lsof_ambiguous):
            result = task_execution._workspace_processes_absent(workspace)
        self.assertFalse(result["verified"])
        self.assertEqual(result["reason"], "process_probe_unavailable")
        self.assertIn("[LOCAL_PATH]", result["probe_evidence"]["stderr"])
        self.assertLessEqual(
            len(result["probe_evidence"]["stderr"].encode("utf-8")),
            task_execution.MAX_PROCESS_PROBE_EVIDENCE_BYTES,
        )

        whitespace_stderr = subprocess.CompletedProcess(["lsof"], 0, "", " \n")
        with mock.patch("scripts.task_agent_execution.subprocess.run", return_value=whitespace_stderr):
            whitespace_result = task_execution._workspace_processes_absent(workspace)
        self.assertFalse(whitespace_result["verified"])
        self.assertEqual(whitespace_result["reason"], "process_probe_unavailable")

    def test_container_control_warnings_and_not_found_text_never_prove_absence(self) -> None:
        prepared = self.prepared_coding()

        def result(
            returncode: int,
            stdout: bytes = b"",
            stderr: bytes = b"",
        ) -> subprocess.CompletedProcess[bytes]:
            return subprocess.CompletedProcess(["docker"], returncode, stdout, stderr)

        for case in (
            "warning_on_exact_identity_list",
            "authorization_plugin_warning_on_inspect",
            "foreign_not_found_on_inspect",
            "generic_not_found_after_remove",
        ):
            with self.subTest(case=case):
                boundary, _proc, _description = self.finished_process_boundary(
                    prepared,
                    f"container-probe-{case}",
                )
                if case == "warning_on_exact_identity_list":
                    boundary.cidfile.unlink()
                container_id = "a" * 64
                inspected = json.dumps(
                    [
                        {
                            "Id": container_id,
                            "Name": f"/{boundary.container_name}",
                            "Config": {
                                "Labels": {
                                    "io.contextlattice.task-isolation": "true",
                                    "io.contextlattice.run-nonce": boundary.run_nonce,
                                    "io.contextlattice.task-ref": boundary.task_ref,
                                }
                            },
                        }
                    ],
                    separators=(",", ":"),
                ).encode("utf-8")
                def docker_control(argv, **_kwargs):
                    command = tuple(str(item) for item in argv[1:])
                    if command[:3] == ("container", "ls", "--all"):
                        if case in {
                            "warning_on_exact_identity_list",
                            "authorization_plugin_warning_on_inspect",
                        }:
                            warning = (
                                b"WARNING: daemon state is stale\n"
                                if case == "warning_on_exact_identity_list"
                                else b"authorization plugin warning: policy unavailable\n"
                            )
                            return result(0, (container_id + "\n").encode("ascii"), warning)
                        if case == "generic_not_found_after_remove":
                            return result(1, b"", b"not found\n")
                        return result(0, (container_id + "\n").encode("ascii"))
                    if command[:2] == ("container", "inspect"):
                        if case == "warning_on_exact_identity_list":
                            return result(0, inspected, b"WARNING: daemon state is stale\n")
                        if case == "authorization_plugin_warning_on_inspect":
                            return result(0, inspected, b"authorization plugin warning: policy unavailable\n")
                        if case == "foreign_not_found_on_inspect":
                            return result(1, b"", b"foreign daemon route not found\n")
                        return result(0, inspected)
                    if command[:3] == ("container", "rm", "--force"):
                        return result(0, (container_id + "\n").encode("ascii"))
                    self.fail(f"unexpected Docker control command: {command!r}")

                with mock.patch(
                    "scripts.task_agent_execution.subprocess.run",
                    side_effect=docker_control,
                ):
                    removal = task_execution._remove_container(boundary)
                self.assertFalse(removal["verified"], removal)
                self.assertTrue(removal["quarantined"], removal)
                self.assertNotEqual(removal["reason"], "container_absent")

    def test_workspace_container_probe_requires_clean_authoritative_empty_lists(self) -> None:
        prepared = self.prepared_coding()

        def result(
            returncode: int,
            stdout: bytes = b"",
            stderr: bytes = b"",
        ) -> subprocess.CompletedProcess[bytes]:
            return subprocess.CompletedProcess(["docker"], returncode, stdout, stderr)

        clean_empty = result(0)
        with (
            mock.patch("scripts.task_agent_execution._docker_executable", return_value=Path("/bin/true")),
            mock.patch("scripts.task_agent_execution._orbstack_endpoint", return_value="unix:///private/orbstack.sock"),
            mock.patch(
                "scripts.task_agent_execution.subprocess.run",
                side_effect=(clean_empty, clean_empty),
            ),
        ):
            absent = task_execution._workspace_containers_absent(self.worktree_root, prepared)
        self.assertTrue(absent["verified"], absent)
        self.assertEqual(absent["reason"], "container_absent")

        for case, ambiguous in (
            (
                "warning",
                result(0, b"", b"WARNING: daemon state may be incomplete\n"),
            ),
            (
                "authorization_plugin",
                result(0, b"", b"authorization plugin unavailable\n"),
            ),
            (
                "foreign_error",
                result(1, b"", b"foreign service not found\n"),
            ),
        ):
            with self.subTest(case=case):
                with (
                    mock.patch("scripts.task_agent_execution._docker_executable", return_value=Path("/bin/true")),
                    mock.patch("scripts.task_agent_execution._orbstack_endpoint", return_value="unix:///private/orbstack.sock"),
                    mock.patch(
                        "scripts.task_agent_execution.subprocess.run",
                        side_effect=(ambiguous, clean_empty),
                    ),
                ):
                    unavailable = task_execution._workspace_containers_absent(
                        self.worktree_root,
                        prepared,
                    )
                self.assertFalse(unavailable["verified"], unavailable)
                self.assertEqual(unavailable["reason"], "container_probe_unavailable")
                self.assertIn("probe_evidence", unavailable)

    def test_ambiguous_ps_probe_never_authorizes_term_or_kill(self) -> None:
        fake_proc = mock.Mock()
        fake_proc.pid = 424249
        fake_proc.poll.return_value = None
        fake_proc.wait.return_value = 0
        ambiguous = subprocess.CompletedProcess(
            ["ps"],
            0,
            f"{fake_proc.pid} {os.getpid()} {fake_proc.pid} /bin/true\n",
            "ps: permission denied\n",
        )
        with (
            mock.patch("scripts.task_agent_execution._reap_and_describe_direct_leader", return_value=(
                {
                    "pid": fake_proc.pid,
                    "ppid": os.getpid(),
                    "pgid": fake_proc.pid,
                    "command": "/bin/true",
                    "argv": ["/bin/true"],
                    "executable": "/bin/true",
                    "cwd": str(self.temp_dir),
                    "_probe_status": "present",
                },
                False,
            )),
            mock.patch("scripts.task_agent_execution.subprocess.run", return_value=ambiguous),
            mock.patch("scripts.task_agent_execution.os.killpg") as killpg,
        ):
            result = terminate_owned_process_group(
                fake_proc,
                expected_executable="/bin/true",
                expected_cwd=self.temp_dir,
                term_timeout=0.01,
                known_pgid=fake_proc.pid,
            )
        self.assertFalse(result["verified"])
        self.assertEqual(result["reason"], "process_group_probe_unavailable")
        self.assertEqual(result["probe_evidence"]["stderr"].strip(), "ps: permission denied")
        killpg.assert_not_called()

    def test_ambiguous_lsof_probe_retains_workspace_and_blocks_cleanup_authority(self) -> None:
        fence = LeaseFence("task-ambiguous-lsof", "attempt-ambiguous-lsof", "lease", "worker-u3", "instance-u3", 2)
        workspace = prepare_workspace(
            {"execution_kind": "non_repo"},
            fence,
            self.profile("local-model"),
            worktree_root=self.worktree_root,
        )
        prepared = PreparedExecution(
            {},
            {},
            fence,
            "local-model",
            self.profile("local-model"),
            SnapshotBinding("", "", "", fence.task_id, fence.attempt_id, {}),
            workspace,
            None,
            {},
            "",
        )
        manifest = result_manifest(
            prepared,
            "observed",
            "",
            [],
            "publication-" + hashlib.sha256(f"{fence.task_id}\0{fence.attempt_id}\0publication".encode()).hexdigest()[:32],
        )
        response = self.publication_response(prepared, manifest)
        ambiguous = {
            "verified": False,
            "reason": "process_probe_unavailable",
            "probe_evidence": {
                "tool": "lsof",
                "returncode": 0,
                "stderr": "lsof: permission denied",
            },
        }
        with (
            mock.patch("scripts.task_agent_execution._workspace_processes_absent", return_value=ambiguous),
            mock.patch(
                "scripts.task_agent_execution._workspace_containers_absent",
                return_value={"verified": True, "reason": "container_absent", "task_ref": "opaque"},
            ),
            mock.patch("scripts.task_agent_execution.cleanup_workspace_after_receipt") as cleanup,
            mock.patch("scripts.task_agent_execution.report_cleanup_receipt") as report,
        ):
            reconciled = reconcile_owned_workspaces(
                orchestrator_url="http://gateway.invalid",
                worker="worker-u3",
                worker_instance="instance-u3",
                get_json=lambda *_args, **_kwargs: self.restart_publication_response(response),
                post_json=lambda *_args, **_kwargs: self.fail("ambiguous lsof probe must not report cleanup"),
                worktree_root=self.worktree_root,
            )
        self.assertEqual(reconciled["cleaned"], [])
        self.assertEqual(reconciled["retained"][0]["reason"], "reconciliation_liveness_unverified")
        self.assertEqual(
            reconciled["retained"][0]["evidence"]["process"]["probe_evidence"]["stderr"],
            "lsof: permission denied",
        )
        self.assertTrue(workspace.cwd.exists())
        cleanup.assert_not_called()
        report.assert_not_called()

    def test_fast_exit_reap_keeps_strict_foreign_descendant_rejection(self) -> None:
        fake_proc = mock.Mock()
        fake_proc.pid = 424241
        fake_proc.poll.return_value = 0
        fake_proc.wait.return_value = 0
        foreign = {
            "pid": 424242,
            "ppid": fake_proc.pid,
            "pgid": fake_proc.pid,
            "command": "/bin/sleep 60",
        }
        with (
            mock.patch("scripts.task_agent_execution._process_description", return_value=None),
            mock.patch("scripts.task_agent_execution._group_members", return_value=[foreign]),
            mock.patch("scripts.task_agent_execution.os.killpg") as killpg,
        ):
            result = terminate_owned_process_group(
                fake_proc,
                expected_executable="/bin/true",
                expected_cwd=self.temp_dir,
                term_timeout=1.0,
                known_pgid=fake_proc.pid,
                tracked_pids={fake_proc.pid},
            )
        self.assertFalse(result["verified"])
        self.assertEqual(result["reason"], "untracked_descendant")
        killpg.assert_not_called()

    def test_matching_child_is_rejected_when_descendants_are_disabled(self) -> None:
        fake_proc = mock.Mock()
        fake_proc.pid = 424246
        leader = {
            "pid": fake_proc.pid,
            "ppid": os.getpid(),
            "pgid": fake_proc.pid,
            "command": "/bin/true run --label=io.contextlattice.run-nonce=nonce-u3 /workspace/fixture",
            "argv": ["/bin/true", "run", "--label=io.contextlattice.run-nonce=nonce-u3", "/workspace/fixture"],
            "executable": "/bin/true",
            "cwd": str(self.temp_dir),
            "_probe_status": "present",
        }
        matching_child = {
            "pid": fake_proc.pid + 1,
            "ppid": fake_proc.pid,
            "pgid": fake_proc.pid,
            "command": leader["command"],
            "argv": list(leader["argv"]),
        }
        with (
            mock.patch("scripts.task_agent_execution._reap_and_describe_direct_leader", return_value=(leader, False)),
            mock.patch("scripts.task_agent_execution._group_members", return_value=[leader, matching_child]),
            mock.patch("scripts.task_agent_execution.os.killpg") as killpg,
        ):
            result = terminate_owned_process_group(
                fake_proc,
                expected_executable="/bin/true",
                expected_cwd=self.temp_dir,
                term_timeout=0.1,
                known_pgid=fake_proc.pid,
                expected_child_executable="/workspace/fixture",
                expected_run_nonce="nonce-u3",
            )
        self.assertFalse(result["verified"])
        self.assertEqual(result["reason"], "untracked_descendant")
        killpg.assert_not_called()

    def test_each_group_signal_requires_a_fresh_full_attestation(self) -> None:
        fake_proc = mock.Mock()
        fake_proc.pid = 424247
        leader = {
            "pid": fake_proc.pid,
            "ppid": os.getpid(),
            "pgid": fake_proc.pid,
            "command": "/bin/true run",
            "argv": ["/bin/true", "run"],
            "executable": "/bin/true",
            "cwd": str(self.temp_dir),
            "_probe_status": "present",
        }
        members = {"pid": fake_proc.pid}
        signals: list[int] = []

        def group_snapshot(_pgid: int) -> tuple[bool, list[dict[str, int]]]:
            if signal.SIGKILL in signals:
                return True, []
            return True, [members]

        def record_signal(_pgid: int, signum: int) -> None:
            signals.append(signum)

        with (
            mock.patch(
                "scripts.task_agent_execution._reap_and_describe_direct_leader",
                side_effect=((leader, False), (leader, False), (leader, False), (leader, False)),
            ),
            mock.patch("scripts.task_agent_execution._group_snapshot", side_effect=group_snapshot),
            mock.patch("scripts.task_agent_execution._descendants", return_value=[]),
            mock.patch("scripts.task_agent_execution.os.killpg", side_effect=record_signal) as killpg,
        ):
            result = terminate_owned_process_group(
                fake_proc,
                expected_executable="/bin/true",
                expected_cwd=self.temp_dir,
                term_timeout=0.01,
                known_pgid=fake_proc.pid,
            )
        self.assertTrue(result["verified"])
        self.assertEqual([call.args for call in killpg.call_args_list], [(fake_proc.pid, signal.SIGTERM), (fake_proc.pid, signal.SIGKILL)])

    def test_disappeared_owned_leader_never_signals_reused_process_group(self) -> None:
        fake_proc = mock.Mock()
        fake_proc.pid = 424243
        owned = {
            "pid": fake_proc.pid,
            "ppid": os.getpid(),
            "pgid": fake_proc.pid,
            "command": "/bin/true run /workspace/fixture",
            "cwd": str(self.temp_dir),
        }
        foreign = {
            "pid": fake_proc.pid + 1,
            "ppid": 1,
            "pgid": fake_proc.pid,
            "command": "/bin/true run /workspace/fixture",
            "cwd": str(self.temp_dir),
        }
        with (
            mock.patch(
                "scripts.task_agent_execution._reap_and_describe_direct_leader",
                side_effect=((owned, False), (None, True)),
            ),
            mock.patch("scripts.task_agent_execution._group_members", return_value=[foreign]),
            mock.patch("scripts.task_agent_execution._process_description", return_value=foreign),
            mock.patch("scripts.task_agent_execution.os.killpg", side_effect=ProcessLookupError) as killpg,
        ):
            result = terminate_owned_process_group(
                fake_proc,
                expected_executable="/bin/true",
                expected_child_executable="/workspace/fixture",
                expected_cwd=self.temp_dir,
                term_timeout=0.01,
                known_pgid=fake_proc.pid,
                tracked_pids={fake_proc.pid, fake_proc.pid + 1},
            )
        self.assertFalse(result["verified"])
        self.assertTrue(result["quarantined"])
        self.assertEqual(result["reason"], "process_identity_unavailable")
        killpg.assert_not_called()

    def test_oci_file_size_limit_stops_oversize_write_and_bounds_workspace(self) -> None:
        prepared = self.prepared_coding()
        max_file_bytes = 32768
        prepared.profile["resource_limits"]["max_file_bytes"] = max_file_bytes  # type: ignore[index]
        prepared.profile["resource_limits"]["max_workspace_bytes"] = 1048576  # type: ignore[index]
        prepared.profile["resource_limits"]["max_workspace_files"] = 128  # type: ignore[index]
        script = prepared.workspace.cwd / "oversize-write.sh"
        script.write_text(
            "#!/bin/sh\nset -eu\ndd if=/dev/zero of=oversize.bin bs=65536 count=2 >/dev/null 2>&1\n",
            encoding="utf-8",
        )
        script.chmod(0o755)

        capture = run_bounded_process(
            ["/workspace/oversize-write.sh"],
            prepared.env,
            prepared.workspace.cwd,
            10,
            profile=prepared.profile,
        )

        self.assertNotEqual(capture.returncode, 0)
        self.assertTrue(capture.termination.get("container", {}).get("verified"))
        self.assertLessEqual((prepared.workspace.cwd / "oversize.bin").stat().st_size, max_file_bytes)
        usage = capture.termination["workspace_usage"]
        self.assertLessEqual(usage["largest_file_bytes"], max_file_bytes)
        self.assertLessEqual(usage["bytes"], 1048576)

    def test_image_inspect_stderr_is_ambiguous_and_exactly_cleans_helper_config(self) -> None:
        prepared = self.prepared_coding()
        secret = b"U3-IMAGE-PROBE-SECRET"
        cases = {
            "warning": b"WARNING: daemon metadata is stale at /Users/private/image.json\n",
            "whitespace": b" \n",
            "malformed_and_oversize": b"\xff API_KEY="
            + secret
            + b" /Users/private/token.json "
            + b"\xff" * (task_execution.MAX_PROCESS_PROBE_EVIDENCE_BYTES + 512),
        }

        for case, probe_stderr in cases.items():
            with self.subTest(case=case):
                captured_config: list[Path] = []

                def image_inspect(argv, **kwargs):
                    self.assertEqual(tuple(str(item) for item in argv[1:3]), ("image", "inspect"))
                    docker_env = kwargs["env"]
                    captured_config.append(Path(docker_env["DOCKER_CONFIG"]))
                    return subprocess.CompletedProcess(argv, 0, b"", probe_stderr)

                with (
                    mock.patch("scripts.task_agent_execution._docker_executable", return_value=Path("/bin/true")),
                    mock.patch("scripts.task_agent_execution._orbstack_endpoint", return_value="unix:///private/orbstack.sock"),
                    mock.patch("scripts.task_agent_execution.subprocess.run", side_effect=image_inspect),
                    mock.patch(
                        "scripts.task_agent_execution._remove_exact_owned_directory",
                        wraps=task_execution._remove_exact_owned_directory,
                    ) as cleanup,
                ):
                    with self.assertRaises(ExecutionBlocked) as blocked:
                        task_execution._container_boundary(
                            prepared.profile,
                            prepared.argv or [],
                            prepared.env,
                            prepared.workspace.cwd,
                        )

                self.assertEqual(blocked.exception.reason, "boundary_image_unavailable")
                self.assertEqual(len(captured_config), 1)
                config_dir = captured_config[0]
                self.assertFalse(config_dir.exists())
                cleanup.assert_called_once()
                self.assertEqual(cleanup.call_args.args[:2], (config_dir, config_dir.parent))
                self.assertEqual(cleanup.call_args.kwargs["reason"], "boundary_config")
                self.assertTrue(cleanup.call_args.kwargs["name_prefix"].startswith("contextlattice-task-"))
                evidence = blocked.exception.evidence
                self.assertTrue(evidence["config"]["verified"])
                probe = evidence["probe"]
                self.assertEqual(probe["tool"], "docker image inspect")
                self.assertEqual(probe["returncode"], 0)
                self.assertTrue(probe["stderr"])
                self.assertLessEqual(
                    len(probe["stderr"].encode("utf-8")),
                    task_execution.MAX_PROCESS_PROBE_EVIDENCE_BYTES,
                )
                self.assertNotIn(secret.decode("ascii"), probe["stderr"])
                self.assertNotIn("/Users/private", probe["stderr"])
                runtime_root = self.worktree_root / ".runtime"
                self.assertFalse(runtime_root.exists() and any(runtime_root.iterdir()))

    def test_real_orbstack_anonymous_config_runs_cached_image_and_cleans(self) -> None:
        prepared = self.prepared_coding()
        docker = shutil.which("docker")
        self.assertIsNotNone(docker)
        anonymous_config = self.new_root("anonymous-empty-docker-config")
        self.assertEqual(list(anonymous_config.iterdir()), [])
        poisoned = {
            "DOCKER_CONFIG": str(anonymous_config),
            "DOCKER_CONTEXT": "foreign-context-that-must-not-be-read",
            "DOCKER_HOST": "tcp://127.0.0.1:1",
            "DOCKER_AUTH_CONFIG": '{"auths":{"private.invalid":{"auth":"unused"}}}',
        }
        with mock.patch.dict(os.environ, poisoned, clear=False):
            endpoint = task_execution._orbstack_endpoint(Path(str(docker)), 5.0)
            self.assertRegex(endpoint, r"^unix://.+/\.orbstack/run/docker\.sock$")
            capture = run_bounded_process(
                prepared.argv or [],
                prepared.env,
                prepared.workspace.cwd,
                10,
                profile=prepared.profile,
            )
        self.assertEqual(capture.returncode, 0, capture.stderr.decode(errors="replace"))
        self.assertEqual(capture.outcome, "execution_observed")
        self.assertTrue(capture.termination.get("container", {}).get("verified"), capture.termination)
        self.assertEqual(list(anonymous_config.iterdir()), [])
        runtime_root = self.worktree_root / ".runtime"
        self.assertFalse(runtime_root.exists() and any(runtime_root.iterdir()))

    def test_workspace_file_count_guard_fails_closed_after_fast_runner_exit(self) -> None:
        prepared = self.prepared_coding()
        script = prepared.workspace.cwd / "file-count-write.sh"
        script.write_text(
            "#!/bin/sh\nset -eu\ni=0\nwhile [ \"$i\" -lt 20 ]; do\n  : > \"entry-$i\"\n  i=$((i + 1))\ndone\n",
            encoding="utf-8",
        )
        script.chmod(0o755)
        baseline_files = sum(1 for path in prepared.workspace.cwd.rglob("*") if not path.is_dir())
        prepared.profile["resource_limits"]["max_workspace_files"] = baseline_files + 2  # type: ignore[index]
        prepared.profile["resource_limits"]["max_workspace_bytes"] = 1048576  # type: ignore[index]

        capture = run_bounded_process(
            ["/workspace/file-count-write.sh"],
            prepared.env,
            prepared.workspace.cwd,
            10,
            profile=prepared.profile,
        )

        self.assertEqual(
            capture.returncode,
            153,
            {"capture": capture, "files": sorted(path.name for path in prepared.workspace.cwd.glob("entry-*"))},
        )
        self.assertEqual(capture.outcome, "resource_limit_exceeded", capture)
        self.assertEqual(capture.termination["resource_reason"], "workspace_file_count_exceeded")
        self.assertTrue(capture.termination.get("container", {}).get("verified"))
        final_files = sum(1 for path in prepared.workspace.cwd.rglob("*") if not path.is_dir())
        self.assertLessEqual(final_files, baseline_files + 20)

    def test_container_name_collision_never_removes_preexisting_container(self) -> None:
        prepared = self.prepared_coding()
        docker = shutil.which("docker")
        self.assertIsNotNone(docker)
        endpoint = task_execution._orbstack_endpoint(Path(str(docker)), 5.0)
        docker_config = self.new_root("collision-docker-config")
        (docker_config / "config.json").write_text('{"auths":{}}\n', encoding="utf-8")
        (docker_config / "config.json").chmod(0o600)
        docker_env = {
            "PATH": "/usr/local/bin:/usr/bin:/bin",
            "HOME": "/var/empty",
            "DOCKER_CONFIG": str(docker_config),
            "DOCKER_HOST": endpoint,
            "DOCKER_CLI_HINTS": "false",
        }
        fixed_nonce = "ab" * 16
        task_ref = hashlib.sha256(
            f"{prepared.fence.task_id}\0{prepared.fence.attempt_id}\0{prepared.workspace.cwd.resolve()}".encode("utf-8")
        ).hexdigest()[:24]
        colliding_name = f"contextlattice-task-{task_ref}-{fixed_nonce[:16]}"
        image = str(prepared.profile["sandbox"]["image"])  # type: ignore[index]
        started = subprocess.run(
            [
                str(docker),
                "run",
                "--detach",
                "--pull=never",
                f"--name={colliding_name}",
                "--network=none",
                "--read-only",
                "--cap-drop=ALL",
                "--security-opt=no-new-privileges",
                "--pids-limit=8",
                "--memory=33554432",
                "--memory-swap=33554432",
                "--cpus=0.1",
                "--label=io.contextlattice.run-nonce=preexisting",
                image,
                "/bin/sleep",
                "60",
            ],
            env=docker_env,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(started.returncode, 0, started.stderr)
        container_id = started.stdout.strip()
        self.assertRegex(container_id, r"^[0-9a-f]{64}$")
        try:
            with mock.patch("scripts.task_agent_execution.secrets.token_hex", return_value=fixed_nonce):
                capture = run_bounded_process(
                    prepared.argv or [],
                    prepared.env,
                    prepared.workspace.cwd,
                    10,
                    profile=prepared.profile,
                )
            self.assertNotEqual(capture.returncode, 0)
            self.assertTrue(capture.termination.get("container", {}).get("verified"))
            self.assertEqual(capture.termination.get("container", {}).get("reason"), "container_not_created")
            still_present = subprocess.run(
                [str(docker), "container", "inspect", container_id],
                env=docker_env,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.PIPE,
                check=False,
            )
            self.assertEqual(still_present.returncode, 0, still_present.stderr.decode(errors="replace"))
        finally:
            subprocess.run(
                [str(docker), "container", "rm", "--force", container_id],
                env=docker_env,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )

    def test_real_orbstack_boundary_blocks_host_paths_and_egress(self) -> None:
        prepared = self.prepared_coding()
        denied_root = self.temp_dir / "host-state"
        denied_root.mkdir()
        for name, env_key in (
            ("home", "BOUNDARY_DENIED_HOME_PATH"),
            ("keychain", "BOUNDARY_DENIED_KEYCHAIN_PATH"),
            ("ssh", "BOUNDARY_DENIED_SSH_PATH"),
            ("docker", "BOUNDARY_DENIED_DOCKER_PATH"),
            ("config", "BOUNDARY_DENIED_CONFIG_PATH"),
        ):
            marker = denied_root / name
            marker.write_text("host-only\n", encoding="utf-8")
            prepared.env[env_key] = str(marker)
        self.assertIsNotNone(prepared.argv)
        self.assertEqual(prepared.argv, ["/workspace/scripts/task_agent_coding_fixture.sh"])
        capture = run_bounded_process(
            prepared.argv or [],
            prepared.env,
            prepared.workspace.cwd,
            int(prepared.profile["resource_limits"]["max_runtime_secs"]),  # type: ignore[index]
            profile=prepared.profile,
        )
        self.assertEqual(capture.returncode, 0, capture.stderr.decode(errors="replace"))
        self.assertEqual(capture.outcome, "execution_observed")
        self.assertTrue(capture.termination.get("verified"))
        self.assertTrue(capture.termination.get("container", {}).get("verified"))
        output = capture.stdout.decode("utf-8", errors="replace")
        self.assertIn('"boundary":"orbstack_oci"', output)
        self.assertIn('"network_egress":"blocked"', output)
        self.assertNotIn(str(self.temp_dir), output)
        result_file = prepared.workspace.cwd / "runner-fixture-result.txt"
        result_text = result_file.read_text(encoding="utf-8")
        for assertion in (
            "host_home=blocked",
            "host_keychain=blocked",
            "host_ssh=blocked",
            "host_docker=blocked",
            "host_config=blocked",
            "root_filesystem=read_only",
            "network_egress=blocked",
        ):
            self.assertIn(assertion, result_text)

    def test_real_orbstack_fast_exit_attestation_stress_has_no_false_quarantine(self) -> None:
        prepared = self.prepared_coding()
        runtime_secs = int(prepared.profile["resource_limits"]["max_runtime_secs"])  # type: ignore[index]
        for attempt in range(16):
            with self.subTest(attempt=attempt):
                capture = run_bounded_process(
                    prepared.argv or [],
                    prepared.env,
                    prepared.workspace.cwd,
                    runtime_secs,
                    profile=prepared.profile,
                )
                self.assertEqual(capture.returncode, 0, capture.stderr.decode(errors="replace"))
                self.assertEqual(capture.outcome, "execution_observed", capture.termination)
                self.assertTrue(capture.termination.get("verified"), capture.termination)
                self.assertTrue(capture.termination.get("container", {}).get("verified"), capture.termination)
        runtime_root = self.worktree_root / ".runtime"
        self.assertFalse(runtime_root.exists() and any(runtime_root.iterdir()))

    def test_owned_container_terminates_on_lease_loss_without_skip(self) -> None:
        prepared = self.prepared_coding()
        script = prepared.workspace.cwd / "lease-probe.sh"
        marker = prepared.workspace.cwd / "lease-started"
        script.write_text("#!/bin/sh\nset -eu\n: > lease-started\n/bin/sleep 60\n", encoding="utf-8")
        script.chmod(0o755)
        lost = threading.Event()
        result: dict[str, object] = {}

        def run() -> None:
            result["capture"] = run_bounded_process(
                ["/workspace/lease-probe.sh"],
                prepared.env,
                prepared.workspace.cwd,
                30,
                profile=prepared.profile,
                lease_lost=lost,
            )

        thread = threading.Thread(target=run)
        thread.start()
        deadline = time.monotonic() + 10
        while not marker.exists() and time.monotonic() < deadline:
            time.sleep(0.02)
        self.assertTrue(marker.exists())
        configs = list((self.worktree_root / ".runtime").glob("*/config.json"))
        self.assertEqual(len(configs), 1)
        self.assertEqual(json.loads(configs[0].read_text(encoding="utf-8")), {"auths": {}})
        self.assertEqual(configs[0].stat().st_mode & 0o777, 0o600)
        lost.set()
        thread.join(15)
        self.assertFalse(thread.is_alive())
        capture = result["capture"]
        self.assertEqual(capture.outcome, "lease_lost", capture.termination)  # type: ignore[union-attr]
        self.assertTrue(capture.termination.get("verified"))  # type: ignore[union-attr]
        self.assertTrue(capture.termination.get("container", {}).get("verified"))  # type: ignore[union-attr]
        self.assertFalse((self.worktree_root / ".runtime").exists())

    def test_container_descendant_cannot_survive_normal_exit_without_skip(self) -> None:
        prepared = self.prepared_coding()
        script = prepared.workspace.cwd / "descendant-probe.sh"
        script.write_text("#!/bin/sh\nset -eu\n/bin/sleep 60 &\nprintf '%s\\n' \"$!\" > child.pid\nexit 0\n", encoding="utf-8")
        script.chmod(0o755)
        capture = run_bounded_process(
            ["/workspace/descendant-probe.sh"],
            prepared.env,
            prepared.workspace.cwd,
            10,
            profile=prepared.profile,
        )
        self.assertEqual(capture.returncode, 0, capture.stderr.decode(errors="replace"))
        self.assertEqual(capture.outcome, "execution_observed")
        self.assertTrue(capture.termination.get("container", {}).get("verified"))

    def test_generic_publication_ack_never_cleans_or_reports_success(self) -> None:
        prepared = self.prepared_coding()
        posts: list[str] = []

        def post_json(_base: str, path: str, _payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            posts.append(path)
            if path.endswith("/publish"):
                return {"ok": True, "acknowledged": True, "status": "writeback_pending"}
            return {"ok": True, "status": "running"}

        with self.assertRaises(PublicationNotAcknowledged):
            execute_prepared(prepared, orchestrator_url="http://gateway.invalid", post_json=post_json)
        self.assertTrue(prepared.workspace.cwd.exists())
        self.assertTrue((self.worktree_root / prepared.fence.task_id / f"{prepared.fence.attempt_id}.owner.json").exists())
        self.assertFalse(any(path.endswith("/cleanup") for path in posts))

    def test_execute_prepared_accepts_committed_idempotent_post_replay(self) -> None:
        prepared = self.prepared_coding()
        posts: list[str] = []

        def post_json(_base: str, path: str, payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            posts.append(path)
            if path.endswith("/publish"):
                return self.route_publication_response(
                    prepared,
                    payload["result"],  # type: ignore[arg-type]
                    status="committed",
                    writeback_status="committed",
                )
            if path.endswith("/cleanup"):
                receipt = copy.deepcopy(payload["cleanup_receipt"])
                receipt.update({"recorded": True, "durable": True, "acknowledged": True})  # type: ignore[union-attr]
                return {"ok": True, "cleanup_receipt": receipt}
            return {"ok": True, "status": "running"}

        result = execute_prepared(
            prepared,
            orchestrator_url="http://gateway.invalid",
            post_json=post_json,
        )
        self.assertEqual(result["publication"]["status"], "committed")
        self.assertEqual(result["publication"]["writeback_status"], "committed")
        self.assertEqual(result["publication_receipt"]["state"], "staged")
        self.assertTrue(result["cleanup_receipt"]["recorded"])
        self.assertTrue(any(path.endswith("/cleanup") for path in posts))
        self.assertFalse(prepared.workspace.cwd.exists())

    def test_end_to_end_coding_fixture_publishes_fenced_result_truth(self) -> None:
        repo, base = self.git_repo()
        claim = self.claim(profile="local-coding", kind="coding", repo=repo, base_sha=base)
        posts: list[tuple[str, dict[str, object], object]] = []
        fence = LeaseFence("task-u3", "attempt-u3", "lease-u3", "worker-u3", "instance-u3", 3)
        response_prepared = PreparedExecution(
            {},
            {},
            fence,
            "local-coding",
            self.profile("local-coding"),
            SnapshotBinding("", "", "", fence.task_id, fence.attempt_id, {}),
            WorkspaceBinding("non_repo", self.worktree_root, None, None, ""),
            None,
            {},
            "",
        )

        def post_json(_base: str, path: str, payload: dict[str, object], **kwargs: object) -> dict[str, object]:
            posts.append((path, copy.deepcopy(payload), kwargs.get("timeout")))
            if path.endswith("/publish"):
                return self.publication_response(response_prepared, payload["result"])  # type: ignore[arg-type]
            if path.endswith("/cleanup"):
                receipt = copy.deepcopy(payload["cleanup_receipt"])
                receipt.update({"recorded": True, "durable": True, "acknowledged": True})  # type: ignore[union-attr]
                return {"ok": True, "cleanup_receipt": receipt}
            return {"ok": True, "status": "running"}

        result = execute_claimed_task(
            claim,
            worker="worker-u3",
            orchestrator_url="http://gateway.invalid",
            get_json=self.snapshot_getter(claim),
            post_json=post_json,
            source_repo=repo,
            worktree_root=self.worktree_root,
        )
        self.assertEqual(result["status"], "publication_pending", result)
        self.assertTrue(result["execution_observed"])
        self.assertEqual(result["fence"]["worker_instance_id"], "instance-u3")
        self.assertEqual(result["fence"]["generation"], 3)
        truth = result["result"]["coding"]["truth"]
        self.assertEqual(truth["base_sha"], base)
        self.assertIn("runner-fixture-result.txt", truth["changed_paths"])
        self.assertTrue(truth["patch_applies_to_base"])
        self.assertEqual(truth["verified_tree"], truth["final_tree"])
        self.assertEqual(truth["runner_result"]["schema_id"], "runner_result.v1")
        self.assertEqual(truth["runner_result"]["task_id"], "task-u3")
        self.assertEqual(truth["runner_result"]["attempt_id"], "attempt-u3")
        self.assertEqual(truth["runner_result"]["runner_version"], "u3-fixture/2")
        self.assertEqual(truth["runner_result"]["tests"][0]["status"], "passed")
        self.assertTrue(truth["evidence_digest"].startswith("sha256:"))
        self.assertEqual(
            truth["artifact_digests"],
            sorted(artifact["digest"] for artifact in result["result"]["artifacts"]),
        )
        self.assertTrue(result["metadata"]["termination"]["container"]["verified"])
        self.assertTrue(result["cleanup_receipt"]["recorded"])
        self.assertEqual(result["result"]["runtime_policy"]["effective_runtime_secs"], 30)
        self.assertEqual(result["result"]["runtime_policy"]["source"], "registered_profile")
        rendered = json.dumps(result, sort_keys=True)
        self.assertNotIn(str(self.temp_dir), rendered)
        publish = next(item for item in posts if item[0].endswith("/publish"))
        self.assertEqual(publish[1]["fence"]["lease_id"], "lease-u3")  # type: ignore[index]
        self.assertEqual(publish[2], 45.0)
        self.assertTrue(Path(result["result"]["workspace"]["workspace_ref"]).is_absolute() is False)
        self.assertTrue(any(item[0].endswith("/cleanup") for item in posts))
        self.assertFalse((self.worktree_root / "task-u3" / "attempt-u3").exists())
        self.assertFalse((self.worktree_root / "task-u3" / "attempt-u3.owner.json").exists())
        runtime_root = self.worktree_root / ".runtime"
        self.assertFalse(runtime_root.exists() and any(runtime_root.iterdir()))

    def test_runtime_limit_is_profile_owned_and_zero_fails_closed(self) -> None:
        prepared = self.prepared_coding()
        self.assertEqual(prepared.profile["resource_limits"]["max_runtime_secs"], 30)  # type: ignore[index]
        with self.assertRaises(ExecutionBlocked) as invalid:
            run_bounded_process(prepared.argv or [], prepared.env, prepared.workspace.cwd, 0, profile=prepared.profile)
        self.assertEqual(invalid.exception.reason, "runtime_limit_invalid")

        repo, base = self.git_repo()
        claim = self.claim(profile="local-coding", kind="coding", repo=repo, base_sha=base)
        claim["attempt"]["runtime_limit_secs"] = 12  # type: ignore[index]
        from scripts.task_agent_execution import prepare_execution

        attempt_root = self.new_root("runtime-attempt-root")
        attempt_prepared = prepare_execution(
            claim,
            worker="worker-u3",
            orchestrator_url="http://gateway.invalid",
            get_json=self.snapshot_getter(claim),
            source_repo=repo,
            worktree_root=attempt_root,
        )
        self.assertEqual(attempt_prepared.profile["_runtime_policy"]["effective_runtime_secs"], 12)  # type: ignore[index]
        self.assertEqual(attempt_prepared.profile["_runtime_policy"]["source"], "fenced_attempt")  # type: ignore[index]

        zero = self.claim(profile="local-coding", kind="coding", repo=repo, base_sha=base)
        zero["attempt"]["runtime_limit_secs"] = 0  # type: ignore[index]
        with self.assertRaises(ExecutionBlocked) as zero_limit:
            prepare_execution(
                zero,
                worker="worker-u3",
                orchestrator_url="http://gateway.invalid",
                get_json=self.snapshot_getter(zero),
                source_repo=repo,
                worktree_root=attempt_root,
            )
        self.assertEqual(zero_limit.exception.reason, "runtime_limit_denied")

        denied = self.claim(profile="local-coding", kind="coding", repo=repo, base_sha=base)
        denied["attempt"]["runtime_limit_secs"] = 31  # type: ignore[index]
        with self.assertRaises(ExecutionBlocked) as over_profile:
            prepare_execution(
                denied,
                worker="worker-u3",
                orchestrator_url="http://gateway.invalid",
                get_json=self.snapshot_getter(denied),
                source_repo=repo,
                worktree_root=attempt_root,
            )
        self.assertEqual(over_profile.exception.reason, "runtime_limit_denied")

    def test_launcher_requires_portable_server_owned_root(self) -> None:
        source = LAUNCHER.read_text(encoding="utf-8")
        self.assertNotIn("/Volumes/", source)
        self.assertNotIn("/Users/", source)
        env = os.environ.copy()
        env.pop("CONTEXTLATTICE_TASK_WORKTREE_ROOT", None)
        result = subprocess.run(["/bin/zsh", "-f", str(LAUNCHER), "--once"], cwd=REPO_ROOT, env=env, text=True, capture_output=True, check=False)
        self.assertEqual(result.returncode, 2)
        self.assertIn("server-owned task worktree root is required", result.stderr)


if __name__ == "__main__":
    unittest.main()
