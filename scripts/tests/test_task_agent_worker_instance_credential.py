#!/usr/bin/env python3
"""Worker-side proof-of-possession state and header coverage."""

from __future__ import annotations

import importlib.util
import hashlib
import json
import os
import stat
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPTS_DIR = REPO_ROOT / "scripts"
if str(SCRIPTS_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_DIR))


def load_task_worker():
    path = SCRIPTS_DIR / "task_agent_worker.py"
    spec = importlib.util.spec_from_file_location("task_agent_worker_instance_credential_under_test", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("failed to load task_agent_worker.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def registration_fixture(worker, state, credential: str, *, replay: bool = False):
    identity = {
        "schema_id": "agent_worker_identity_readback.v1",
        "contract_version": 1,
        "identity_id": "credential-identity-1",
        "principal_id": "credential-principal",
        "workspace_id": "credential-workspace",
        "requested_worker_id": state["requested_worker_id"],
        "canonical_worker_id": state["requested_worker_id"],
        "worker_instance_id": state["worker_instance_id"],
        "worker_identity_update_generation": 0,
        "acknowledged_generation": 0,
        "requested_id_digest": "",
        "identity_digest": "",
        "status": "active",
        "created_at": "2026-08-11T00:00:00Z",
        "updated_at": "2026-08-11T00:00:00Z",
        "closed_at": "",
        "format_contract": {},
    }
    identity["requested_id_digest"] = "sha256:" + hashlib.sha256(
        worker._canonical_worker_identity_json({"requested_worker_id": identity["requested_worker_id"]})
    ).hexdigest()
    identity["identity_digest"] = worker._worker_identity_identity_digest(identity)
    identity = worker.attach_format_contract("agent_worker_identity_readback.v1", identity)
    response = {
        "schema_id": "agent_worker_identity_registration.v1",
        "contract_version": 1,
        "principal_id": identity["principal_id"],
        "workspace_id": identity["workspace_id"],
        "requested_worker_id": identity["requested_worker_id"],
        "canonical_worker_id": identity["canonical_worker_id"],
        "worker_instance_id": identity["worker_instance_id"],
        "worker_identity_update_generation": 0,
        "identity": identity,
        "identity_update": None,
        "identity_update_required": False,
        "idempotent_replay": replay,
        "format_contract": {},
    }
    response = worker.attach_format_contract("agent_worker_identity_registration.v1", response)
    return response, credential


class TaskAgentWorkerInstanceCredentialTests(unittest.TestCase):
    def test_registration_persists_client_secret_and_scopes_headers(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ), mock.patch.object(worker, "_worker_authority_config", return_value=("", "")):
            state = worker._load_or_create_worker_state("credential-worker", dispatcher_id="credential-dispatcher")
            response, _ = registration_fixture(worker, state, "a" * 64)

            def first_post(*_args, **_kwargs):
                worker._LAST_RESPONSE_HEADERS.set(
                    {worker.WORKER_INSTANCE_CREDENTIAL_HEADER.lower(): "b" * 64, "content-type": "application/json"}
                )
                return response

            with mock.patch.object(worker, "_post", side_effect=first_post):
                registered = worker._register_worker_identity("http://127.0.0.1:8075", state)
            credential = registered["worker_instance_credential"]
            self.assertRegex(credential, r"^[0-9a-f]{64}$")
            state_path = Path(root) / (
                "worker_identity.dispatcher-"
                + hashlib.sha256(b"credential-dispatcher").hexdigest()[:24]
                + ".json"
            )
            raw = state_path.read_text()
            self.assertIn(credential, raw)
            self.assertEqual(stat.S_IMODE(state_path.stat().st_mode), 0o600)
            self.assertNotIn(credential, json.dumps(worker._redact_runner_value(registered)))
            with worker._worker_auth_scope(registered):
                self.assertEqual(
                    worker._worker_auth_headers("/agents/tasks/next"),
                    {
                        "X-Worker-Instance-ID": registered["worker_instance_id"],
                        worker.WORKER_INSTANCE_CREDENTIAL_HEADER: credential,
                    },
                )
                self.assertEqual(
                    worker._worker_auth_headers("/agents/tasks/task-1/heartbeat")[worker.WORKER_INSTANCE_CREDENTIAL_HEADER],
                    credential,
                )
                self.assertEqual(
                    worker._worker_auth_headers("/agents/tasks/task-1/attempts/attempt-1/publication")[worker.WORKER_INSTANCE_CREDENTIAL_HEADER],
                    credential,
                )
                self.assertEqual(worker._worker_auth_headers("/agents/tasks/task-1"), {})
                self.assertEqual(worker._worker_auth_headers("/agents/tasks/task-1/deliveries/delivery-1/ack"), {})
                self.assertEqual(
                    worker._worker_auth_headers("/agents/workers/identity" )[worker.WORKER_INSTANCE_CREDENTIAL_HEADER],
                    credential,
                )
                self.assertEqual(worker._worker_auth_headers("/memory/write"), {})

    def test_lost_registration_response_retries_same_persisted_credential(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ), mock.patch.object(worker, "_worker_authority_config", return_value=("", "")):
            state = worker._load_or_create_worker_state("credential-worker", dispatcher_id="credential-dispatcher")
            response, _ = registration_fixture(worker, state, "b" * 64)
            calls: list[str] = []

            def first_post(*_args, **_kwargs):
                calls.append(worker._worker_auth_headers("/agents/workers/register")[worker.WORKER_INSTANCE_CREDENTIAL_HEADER])
                if len(calls) == 1:
                    raise RuntimeError("transport lost after registration commit")
                return response

            with mock.patch.object(worker, "_post", side_effect=first_post):
                with self.assertRaisesRegex(RuntimeError, "transport lost"):
                    worker._register_worker_identity("http://127.0.0.1:8075", state)
                persisted = state["worker_instance_credential"]
                self.assertRegex(persisted, r"^[0-9a-f]{64}$")
                registered = worker._register_worker_identity("http://127.0.0.1:8075", state)
            self.assertEqual(registered["worker_instance_credential"], persisted)
            self.assertEqual(calls, [persisted, persisted])

    def test_unrelated_registration_conflict_does_not_rotate_state(self) -> None:
        worker = load_task_worker()

        class ConflictClient:
            last_response_status = 409
            last_response_headers: dict[str, str] = {}
            last_error_code = "worker_identity_collision"

            def __init__(self, *_args, **_kwargs):
                self.calls = 0

            def post_json(self, *_args, **_kwargs):
                self.calls += 1
                raise RuntimeError("unrelated registration conflict status=409")

            def close(self) -> None:
                return None

        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ), mock.patch.object(worker, "_worker_authority_config", return_value=("", "")):
            state = worker._load_or_create_worker_state("credential-worker", dispatcher_id="credential-dispatcher")
            original_instance = state["worker_instance_id"]
            with mock.patch.object(worker, "ContextLatticeClient", ConflictClient):
                with self.assertRaisesRegex(RuntimeError, "unrelated registration conflict"):
                    worker._register_worker_identity("http://127.0.0.1:8075", state)
            self.assertEqual(state["worker_instance_id"], original_instance)
            self.assertRegex(state["worker_instance_credential"], r"^[0-9a-f]{64}$")
            state_path = Path(root) / (
                "worker_identity.dispatcher-"
                + hashlib.sha256(b"credential-dispatcher").hexdigest()[:24]
                + ".json"
            )
            persisted = json.loads(state_path.read_text())
            self.assertEqual(persisted["worker_instance_id"], original_instance)
            self.assertEqual(persisted["worker_instance_credential"], state["worker_instance_credential"])

    def test_exact_legacy_migration_challenge_rotates_once_and_retries(self) -> None:
        worker = load_task_worker()

        class MigrationClient:
            calls = 0
            first_credential = ""

            def __init__(self, *_args, **_kwargs):
                self.last_response_status = None
                self.last_response_headers: dict[str, str] = {}
                self.last_error_code = ""
                self.extra_headers = dict(_kwargs.get("extra_headers") or {})

            def post_json(self, *_args, **_kwargs):
                type(self).calls += 1
                if type(self).calls == 1:
                    type(self).first_credential = self.extra_headers.get(
                        worker.WORKER_INSTANCE_CREDENTIAL_HEADER, ""
                    )
                    self.last_response_status = 409
                    self.last_error_code = "worker_identity_credential_migration_required"
                    raise RuntimeError("legacy migration challenge")
                response, _ = registration_fixture(worker, state, state["worker_instance_credential"])
                self.last_response_status = 200
                self.last_error_code = ""
                return response

            def close(self) -> None:
                return None

        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ), mock.patch.object(worker, "_worker_authority_config", return_value=("", "")), mock.patch.object(
            worker, "ContextLatticeClient", MigrationClient
        ):
            state = worker._load_or_create_worker_state("credential-worker", dispatcher_id="migration-dispatcher")
            old_instance = state["worker_instance_id"]
            registered = worker._register_worker_identity("http://127.0.0.1:8075", state)
            old_credential = MigrationClient.first_credential
            self.assertRegex(old_credential, r"^[0-9a-f]{64}$")
            self.assertEqual(MigrationClient.calls, 2)
            self.assertNotEqual(registered["worker_instance_id"], old_instance)
            self.assertNotEqual(registered["worker_instance_credential"], old_credential)
            self.assertRegex(registered["worker_instance_credential"], r"^[0-9a-f]{64}$")
            persisted = json.loads(
                (
                    Path(root)
                    / ("worker_identity.dispatcher-" + hashlib.sha256(b"migration-dispatcher").hexdigest()[:24] + ".json")
                ).read_text()
            )
            self.assertEqual(persisted["worker_instance_id"], registered["worker_instance_id"])
            self.assertNotIn(old_credential, json.dumps(worker._redact_runner_value(persisted)))

    def test_real_heartbeat_thread_uses_explicit_snapshot_without_contextvar(self) -> None:
        worker = load_task_worker()
        from scripts.task_agent_execution import LeaseFence, SnapshotBinding

        snapshot = worker.WorkerAuthSnapshot("heartbeat-instance", "c" * 64)
        fence = LeaseFence("task-1", "attempt-1", "lease-1", "worker-1", "heartbeat-instance", 1)
        binding = SnapshotBinding("snapshot-1", "sha256:" + "1" * 64, "session-1", "task-1", "attempt-1", {})
        stop = threading.Event()
        lost = threading.Event()
        calls: list[tuple[str, object, dict[str, str]]] = []

        def post_json(_url, path, _payload, *, timeout, auth_snapshot=None):
            del timeout
            calls.append((threading.current_thread().name, auth_snapshot, worker._worker_auth_headers(path, auth_snapshot)))
            stop.set()
            return {"ok": True}

        thread = worker.start_lease_heartbeat(
            "http://gateway.invalid",
            fence,
            binding,
            1.0,
            post_json,
            lost,
            stop,
            lease_expires_at="2099-01-01T00:00:00Z",
            auth_snapshot=snapshot,
        )
        thread.join(timeout=3.0)
        self.assertFalse(thread.is_alive())
        self.assertEqual(len(calls), 1)
        name, observed_snapshot, headers = calls[0]
        self.assertTrue(name.startswith("task-heartbeat-"))
        self.assertIs(observed_snapshot, snapshot)
        self.assertEqual(headers[worker.WORKER_INSTANCE_CREDENTIAL_HEADER], "c" * 64)
        self.assertEqual(headers["X-Worker-Instance-ID"], "heartbeat-instance")


if __name__ == "__main__":
    unittest.main()
