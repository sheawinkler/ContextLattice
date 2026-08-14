#!/usr/bin/env python3
"""Regression coverage for external task worker claim identity."""

from __future__ import annotations

import contextlib
import hashlib
import http.server
import importlib.util
import io
import json
import multiprocessing
import os
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import urllib.error
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPTS_DIR = REPO_ROOT / "scripts"
if str(SCRIPTS_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_DIR))


def load_task_worker():
    path = SCRIPTS_DIR / "task_agent_worker.py"
    spec = importlib.util.spec_from_file_location("task_agent_worker_claim_identity_under_test", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("failed to load task_agent_worker.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def update_fixture(worker, state, *, generation: int = 1, canonical: str = "hermes-agent-abc123"):
    update = {
        "schema_id": "agent_worker_identity_update.v1",
        "contract_version": 1,
        "update_id": "update-1",
        "identity_id": "identity-1",
        "principal_id": "principal-1",
        "workspace_id": "workspace-1",
        "worker_instance_id": state["worker_instance_id"],
        "old_worker_id": "hermes-agent",
        "requested_worker_id": "hermes-agent",
        "new_worker_id": canonical,
        "canonical_worker_id": canonical,
        "worker_identity_update_generation": generation,
        "update_digest": "",
        "receipt_digest": "",
        "state": "delivered",
        "delivery_attempts": 1,
        "last_error": "",
        "created_at": "2026-08-11T00:00:00Z",
        "updated_at": "2026-08-11T00:00:00Z",
        "delivered_at": "2026-08-11T00:00:00Z",
        "acknowledged_at": "",
        "ack_receipt_digest": "",
        "expires_at": "",
        "ack_required": True,
        "format_contract": {},
    }
    update["update_digest"] = worker._worker_identity_update_digest(update)
    update["receipt_digest"] = worker._worker_identity_receipt_digest(update)
    update["ack_receipt_digest"] = worker._worker_identity_ack_receipt_digest(update)
    return worker.attach_format_contract("agent_worker_identity_update.v1", update)


def identity_fixture(worker, state, *, generation: int = 0, canonical: str = "hermes-agent", acknowledged: int | None = None):
    identity = {
        "schema_id": "agent_worker_identity_readback.v1",
        "contract_version": 1,
        "identity_id": "identity-1",
        "principal_id": "principal-1",
        "workspace_id": "workspace-1",
        "requested_worker_id": "hermes-agent",
        "canonical_worker_id": canonical,
        "worker_instance_id": state["worker_instance_id"],
        "worker_identity_update_generation": generation,
        "acknowledged_generation": generation if acknowledged is None else acknowledged,
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
    return worker.attach_format_contract("agent_worker_identity_readback.v1", identity)


def registration_fixture(worker, state, *, generation: int = 0, canonical: str = "hermes-agent", acknowledged: int | None = None, update=None):
    identity = identity_fixture(worker, state, generation=generation, canonical=canonical, acknowledged=acknowledged)
    response = {
        "schema_id": "agent_worker_identity_registration.v1",
        "contract_version": 1,
        "principal_id": identity["principal_id"],
        "workspace_id": identity["workspace_id"],
        "requested_worker_id": identity["requested_worker_id"],
        "canonical_worker_id": identity["canonical_worker_id"],
        "worker_instance_id": identity["worker_instance_id"],
        "worker_identity_update_generation": identity["worker_identity_update_generation"],
        "identity": identity,
        "identity_update": update,
        "identity_update_required": update is not None,
        "idempotent_replay": False,
        "format_contract": {},
    }
    return worker.attach_format_contract("agent_worker_identity_registration.v1", response)


def _allocate_state_in_child(root: str, queue, release_event, dispatcher: str) -> None:
    worker = load_task_worker()
    os.environ["TASK_AGENT_WORKER_STATE_ROOT"] = root
    state = worker._load_or_create_worker_state("hermes-agent", dispatcher_id=dispatcher)
    queue.put(state["worker_instance_id"])
    if not release_event.wait(10):
        raise RuntimeError("worker identity slot release timed out")


def _allocate_default_state_in_child(root: str, dispatcher_root: str, queue, release_event) -> None:
    worker = load_task_worker()
    os.environ["TASK_AGENT_WORKER_STATE_ROOT"] = root
    os.environ["CONTEXTLATTICE_TASK_WORKTREE_ROOT"] = dispatcher_root
    state = worker._load_or_create_worker_state("hermes-agent")
    queue.put((state["worker_instance_id"], state.get("dispatcher_id", "")))
    if not release_event.wait(10):
        raise RuntimeError("worker identity slot release timed out")


class TaskAgentWorkerClaimIdentityTests(unittest.TestCase):
    def test_common_emit_and_gateway_errors_use_canonical_sanitizer(self) -> None:
        common = __import__("scripts.agent._common", fromlist=["_common"])
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            common.emit(
                {
                    "message": "key=U3-COMMON-EMIT-KEY /Users/private/output",
                    "nested": {"auth": "U3-COMMON-EMIT-AUTH"},
                },
                pretty=False,
            )
        rendered = output.getvalue()
        self.assertNotIn("U3-COMMON-EMIT-KEY", rendered)
        self.assertNotIn("U3-COMMON-EMIT-AUTH", rendered)
        self.assertNotIn("/Users/private/output", rendered)

        gateway_body = b'{"api":"U3-COMMON-GATEWAY-SECRET","detail":"/Users/private/gateway"}'
        error = urllib.error.HTTPError(
            "http://gateway.invalid/v1/tasks",
            502,
            "bad gateway",
            None,
            io.BytesIO(gateway_body),
        )
        try:
            with mock.patch.object(common.urllib.request, "urlopen", side_effect=error):
                with self.assertRaises(SystemExit) as raised:
                    common.request_json("GET", "/v1/tasks", None, 1.0)
        finally:
            error.close()
        self.assertNotIn("U3-COMMON-GATEWAY-SECRET", str(raised.exception))
        self.assertNotIn("/Users/private/gateway", str(raised.exception))

        class Response:
            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def read(self, _limit=-1):
                return b'{"password":"U3-COMMON-MALFORMED-SECRET"'

        with mock.patch.object(common.urllib.request, "urlopen", return_value=Response()):
            with self.assertRaises(SystemExit) as raised:
                common.request_json("GET", "/v1/tasks", None, 1.0)
        self.assertNotIn("U3-COMMON-MALFORMED-SECRET", str(raised.exception))

        class InvalidUTF8Response:
            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def read(self, _limit=-1):
                return b'{"ok":true,"proof":"\xff"}'

        with mock.patch.object(common.urllib.request, "urlopen", return_value=InvalidUTF8Response()):
            with self.assertRaises(SystemExit) as raised:
                common.request_json_for_validation("GET", "/v1/tasks", None, 1.0)
        self.assertIn("gateway_response_invalid", str(raised.exception))
        self.assertNotIn("\\xff", str(raised.exception))

        class OversizedResponse:
            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def read(self, limit=-1):
                return b'{"ok":true}' + b" " * max(0, limit)

        with mock.patch.object(common.urllib.request, "urlopen", return_value=OversizedResponse()):
            with self.assertRaises(SystemExit) as raised:
                common.request_json_for_validation("GET", "/v1/tasks", None, 1.0)
        self.assertIn("gateway_response_invalid", str(raised.exception))

        class DeepResponse:
            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return False

            def read(self, _limit=-1):
                return ("[" * 500000 + "0" + "]" * 500000).encode("utf-8")

        with mock.patch.object(common.urllib.request, "urlopen", return_value=DeepResponse()):
            with self.assertRaises(SystemExit) as raised:
                common.request_json_for_validation("GET", "/v1/tasks", None, 1.0)
        self.assertIn("gateway_response_invalid", str(raised.exception))

    def test_legacy_runner_environment_is_closed_and_credential_free(self) -> None:
        worker = load_task_worker()
        env = worker._legacy_runner_env(
            {
                "TASK_ID": "task-u3",
                "TASK_API_KEY": "U3-TASK-API-SECRET",
                "OPENAI_API_KEY": "U3-PROVIDER-SECRET",
                "ANTHROPIC_API_KEY": "U3-ANTHROPIC-SECRET",
                "PATH": "/private/operator/bin",
                "UNRELATED_HOST_SETTING": "private-host-value",
            }
        )
        self.assertEqual(env["TASK_ID"], "task-u3")
        self.assertEqual(env["PATH"], "/usr/local/bin:/usr/bin:/bin")
        for key in (
            "TASK_API_KEY",
            "OPENAI_API_KEY",
            "ANTHROPIC_API_KEY",
            "UNRELATED_HOST_SETTING",
        ):
            self.assertNotIn(key, env)
        self.assertNotIn("U3-TASK-API-SECRET", json.dumps(env, sort_keys=True))

    def test_claim_sends_same_trimmed_worker_in_query_and_body(self) -> None:
        worker = load_task_worker()
        captured: dict[str, object] = {}

        def fake_post(orchestrator_url, path, payload, params=None, *, timeout=30.0):
            captured.update(
                orchestrator_url=orchestrator_url,
                path=path,
                payload=payload,
                params=params,
                timeout=timeout,
            )
            return {"task": None}

        with mock.patch.object(worker, "_post", side_effect=fake_post):
            result = worker._claim_next_task("http://127.0.0.1:8075", "  hermes-agent  ")

        self.assertEqual(result, {"task": None})
        self.assertEqual(captured["path"], "/agents/tasks/next")
        self.assertEqual(captured["payload"], {"worker": "hermes-agent"})
        self.assertEqual(captured["params"], {"worker": "hermes-agent"})

    def test_claim_requires_persisted_instance_and_canonical_identity(self) -> None:
        worker = load_task_worker()
        state = {"requested_worker_id": "hermes-agent", "canonical_worker_id": "hermes-agent", "worker_instance_id": ""}
        with self.assertRaisesRegex(RuntimeError, "worker instance is required"):
            worker._claim_next_task("http://127.0.0.1:8075", "  hermes-agent  ", state=state)

    def test_requested_worker_id_uses_public_lease_grammar_before_durable_state(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            for requested in ("bad/worker", "~worker", "é-worker", "worker with spaces"):
                with self.assertRaisesRegex(RuntimeError, "public lease identifier"):
                    worker._load_or_create_worker_state(requested)
            self.assertEqual(list(Path(root).glob("worker_identity*.json")), [])

    def test_main_uses_server_canonical_after_registration_for_reconcile_and_claim(self) -> None:
        worker = load_task_worker()
        state = {
            "requested_worker_id": "hermes-agent",
            "worker_instance_id": "instance-main-canonical",
            "canonical_worker_id": "hermes-agent-canonical",
            "worker_identity_update_generation": 1,
            "acknowledged_generation": 1,
            "principal_id": "principal-main",
            "workspace_id": "workspace-main",
            "worker_instance_credential": "a" * 64,
        }
        captured: list[str] = []
        with mock.patch.object(worker.sys, "argv", ["task_agent_worker.py", "--once", "--worker-instance", "instance-main-canonical"]), mock.patch.object(
            worker, "_load_or_create_worker_state", return_value=dict(state)
        ), mock.patch.object(worker, "_register_worker_identity", return_value=dict(state)), mock.patch.object(
            worker, "reconcile_owned_workspaces", side_effect=lambda **_kwargs: captured.append(_kwargs["worker"])
        ), mock.patch.object(worker, "_claim_next_task", side_effect=lambda _url, worker_id, **_kwargs: (captured.append(worker_id) or {"task": None})), mock.patch.object(
            worker, "_save_worker_state", side_effect=lambda value, *args, **kwargs: dict(value)
        ):
            worker.main()
        self.assertEqual(captured, ["hermes-agent-canonical", "hermes-agent-canonical"])

    def test_retirement_readback_recovers_after_post_response_loss_without_rebinding(self) -> None:
        worker = load_task_worker()
        state = {
            "requested_worker_id": "hermes-agent",
            "worker_instance_id": "instance-retirement-readback",
            "canonical_worker_id": "hermes-agent-canonical",
            "worker_identity_update_generation": 1,
            "acknowledged_generation": 1,
            "identity_id": "identity-retirement-readback",
            "identity_digest": "sha256:" + "a" * 64,
            "principal_id": "principal-retirement-readback",
            "workspace_id": "workspace-retirement-readback",
        }
        receipt = {
            "schema_id": "agent_worker_identity_retirement_receipt.v1",
            "contract_version": 1,
            "retirement_id": "retirement-readback",
            "identity_id": state["identity_id"],
            "principal_id": state["principal_id"],
            "workspace_id": state["workspace_id"],
            "requested_worker_id": state["requested_worker_id"],
            "canonical_worker_id": state["canonical_worker_id"],
            "tombstone_canonical_worker_id": "closed-retirement-readback",
            "worker_instance_id": state["worker_instance_id"],
            "worker_identity_update_generation": 1,
            "acknowledged_generation": 1,
            "identity_digest": state["identity_digest"],
            "closed_identity_digest": "sha256:" + "b" * 64,
            "closed_status": "closed",
            "retirement_digest": worker._worker_identity_retirement_digest(state),
            "retired": True,
            "canonical_reclaimed": True,
            "closed_at": "2026-08-11T00:00:00Z",
            "idempotent_replay": True,
        }
        receipt["retirement_receipt_digest"] = worker._worker_identity_retirement_receipt_digest(receipt)
        receipt = worker.attach_format_contract("agent_worker_identity_retirement_receipt.v1", receipt)
        get_calls: list[dict[str, str]] = []

        def fake_get(_url, _path, params=None, *, timeout=30.0):
            get_calls.append(dict(params or {}))
            if len(get_calls) == 1:
                raise RuntimeError("server unavailable")
            return receipt

        with mock.patch.object(worker, "_get", side_effect=fake_get), mock.patch.object(
            worker, "_post", side_effect=ConnectionError("response lost after server commit")
        ) as post:
            recovered = worker._retire_worker_identity("http://gateway.invalid", state)
        self.assertTrue(recovered["idempotent_replay"])
        self.assertEqual(recovered["retirement_receipt_digest"], receipt["retirement_receipt_digest"])
        self.assertEqual(post.call_count, 1)
        self.assertEqual(len(get_calls), 2)
        self.assertEqual(get_calls[-1]["worker_instance_id"], state["worker_instance_id"])

    def test_owner_only_state_is_stable_across_restart(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            first = worker._load_or_create_worker_state("hermes-agent", dispatcher_id="owner-dispatcher")
            state_path = worker._worker_dispatcher_state_path(Path(root), "owner-dispatcher")
            self.assertTrue(state_path.is_file())
            self.assertEqual(stat.S_IMODE(state_path.stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE(Path(root).stat().st_mode), 0o700)
            self.assertEqual(list(Path(root).glob(".worker-identity-*")), [])
            second = worker._load_or_create_worker_state("hermes-agent", dispatcher_id="owner-dispatcher")
            self.assertEqual(first["worker_instance_id"], second["worker_instance_id"])
            self.assertNotEqual(first["worker_instance_id"], first["requested_worker_id"])
            updated = dict(second)
            updated["canonical_worker_id"] = "hermes-agent-abc123"
            updated["worker_identity_update_generation"] = 1
            worker._save_worker_state(updated)
            restarted = worker._load_or_create_worker_state("hermes-agent", dispatcher_id="owner-dispatcher")
            self.assertEqual(restarted["worker_instance_id"], first["worker_instance_id"])
            self.assertEqual(restarted["canonical_worker_id"], "hermes-agent-abc123")
            self.assertEqual(restarted["worker_identity_update_generation"], 1)
            self.assertEqual(json.loads(state_path.read_text()), restarted)

    def test_persisted_identity_scalars_do_not_coerce(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            state = worker._load_or_create_worker_state("strict-worker")
            path = Path(root) / "worker_identity.json"
            worker._release_worker_state_lock(path)
            raw = json.loads(path.read_text())
            for malformed in (True, 1.5, "1"):
                candidate = dict(raw)
                candidate["worker_identity_update_generation"] = malformed
                path.write_text(json.dumps(candidate, sort_keys=True))
                os.chmod(path, 0o600)
                with self.assertRaisesRegex(RuntimeError, "state persistence failed"):
                    load_task_worker()._load_or_create_worker_state("strict-worker")
            self.assertEqual(state["worker_identity_update_generation"], 0)

    def test_persisted_generation_range_is_bounded(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            worker._load_or_create_worker_state("strict-range-worker")
            path = Path(root) / "worker_identity.json"
            worker._release_worker_state_lock(path)
            raw = json.loads(path.read_text())
            candidate = dict(raw)
            candidate["worker_identity_update_generation"] = worker.WORKER_IDENTITY_GENERATION_MAX + 1
            path.write_text(json.dumps(candidate, sort_keys=True))
            os.chmod(path, 0o600)
            with self.assertRaisesRegex(RuntimeError, "state persistence failed"):
                load_task_worker()._load_or_create_worker_state("strict-range-worker")

    def test_unconfigured_multiple_worker_slots_allocate_fresh_instances(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            first = worker._load_or_create_worker_state("ambiguous-worker")
            first_path = Path(root) / "worker_identity.json"
            worker._release_worker_state_lock(first_path)
            second = load_task_worker()._load_or_create_worker_state("ambiguous-worker")
            self.assertNotEqual(first["worker_instance_id"], second["worker_instance_id"])
            self.assertEqual(second["worker_identity_update_generation"], 0)
            worker._release_worker_state_lock(Path(root) / "worker_identity.1.json")

    def test_unkeyed_clean_retirement_reclaims_slot_without_identity_reuse(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            instances: set[str] = set()
            for _ in range(worker.WORKER_STATE_SLOT_LIMIT * 2):
                state = worker._load_or_create_worker_state("retiring-worker")
                state["principal_id"] = "retiring-principal"
                state["workspace_id"] = "retiring-workspace"
                state["canonical_worker_id"] = "retiring-worker"
                state = worker._save_worker_state(state)
                instances.add(state["worker_instance_id"])
                self.assertTrue(worker._retire_unkeyed_worker_state(state))
            self.assertEqual(len(instances), worker.WORKER_STATE_SLOT_LIMIT * 2)
            self.assertLessEqual(len(list(Path(root).glob("worker_identity*.json"))), 1)

    def test_two_process_same_requested_id_allocates_distinct_instances_and_reversed_restart(self) -> None:
        worker = load_task_worker()
        context = multiprocessing.get_context("spawn")
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            queue = context.Queue()
            release_event = context.Event()
            first = context.Process(target=_allocate_state_in_child, args=(root, queue, release_event, "dispatcher-a"))
            first.start()
            first_instance = queue.get(timeout=10)
            second = context.Process(target=_allocate_state_in_child, args=(root, queue, release_event, "dispatcher-b"))
            second.start()
            second_instance = queue.get(timeout=10)
            release_event.set()
            first.join(10)
            second.join(10)
            self.assertEqual(first.exitcode, 0)
            self.assertEqual(second.exitcode, 0)
            self.assertNotEqual(first_instance, second_instance)
            restarted_b = worker._load_or_create_worker_state("hermes-agent", dispatcher_id="dispatcher-b")
            restarted_a = worker._load_or_create_worker_state("hermes-agent", dispatcher_id="dispatcher-a")
            self.assertEqual(restarted_b["worker_instance_id"], second_instance)
            self.assertEqual(restarted_a["worker_instance_id"], first_instance)
            stable_restart_b = worker._load_or_create_worker_state("hermes-agent", dispatcher_id="dispatcher-b")
            self.assertEqual(stable_restart_b["worker_instance_id"], second_instance)
            self.assertEqual(restarted_b["dispatcher_id"], "dispatcher-b")
            self.assertEqual(restarted_a["dispatcher_id"], "dispatcher-a")
            self.assertEqual(list(Path(root).glob(".worker-identity-*")), [])

    def test_default_launcher_allocates_distinct_instances_and_never_resurrects_unkeyed_state(self) -> None:
        worker = load_task_worker()
        context = multiprocessing.get_context("spawn")
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as worktree_a, tempfile.TemporaryDirectory() as worktree_b, mock.patch.dict(
            os.environ,
            {"TASK_AGENT_WORKER_STATE_ROOT": root, "CONTEXTLATTICE_TASK_WORKTREE_ROOT": worktree_a},
            clear=False,
        ):
            os.environ.pop("TASK_AGENT_DISPATCHER_ID", None)
            os.environ.pop("TASK_WORKER_DISPATCHER_ID", None)
            os.environ.pop("CONTEXTLATTICE_TASK_WORKER_DISPATCHER_ID", None)
            queue = context.Queue()
            release_event = context.Event()
            first = context.Process(target=_allocate_default_state_in_child, args=(root, worktree_a, queue, release_event))
            first.start()
            first_instance, first_dispatcher = queue.get(timeout=10)
            second = context.Process(target=_allocate_default_state_in_child, args=(root, worktree_b, queue, release_event))
            second.start()
            second_instance, second_dispatcher = queue.get(timeout=10)
            release_event.set()
            first.join(10)
            second.join(10)
            self.assertEqual(first.exitcode, 0)
            self.assertEqual(second.exitcode, 0)
            self.assertNotEqual(first_instance, second_instance)
            self.assertEqual(first_dispatcher, "")
            self.assertEqual(second_dispatcher, "")
            os.environ["CONTEXTLATTICE_TASK_WORKTREE_ROOT"] = worktree_b
            restarted_b = worker._load_or_create_worker_state("hermes-agent")
            os.environ["CONTEXTLATTICE_TASK_WORKTREE_ROOT"] = worktree_a
            restarted_a = worker._load_or_create_worker_state("hermes-agent")
            self.assertNotIn(restarted_b["worker_instance_id"], {first_instance, second_instance})
            self.assertNotIn(restarted_a["worker_instance_id"], {first_instance, second_instance, restarted_b["worker_instance_id"]})
            self.assertNotIn("dispatcher_id", restarted_b)
            self.assertNotIn("dispatcher_id", restarted_a)
            worker._release_worker_state_lock(worker._worker_state_path(Path(root)))
            worker._release_worker_state_lock(Path(root) / "worker_identity.1.json")
            worker._release_worker_state_lock(Path(root) / "worker_identity.2.json")

    def test_launcher_wires_dispatcher_root_and_explicit_override(self) -> None:
        source = (REPO_ROOT / "scripts" / "launch_task_agent.sh").read_text(encoding="utf-8")
        self.assertIn("CONTEXTLATTICE_TASK_WORKTREE_ROOT", source)
        self.assertIn("--dispatcher-id", source)
        self.assertIn("WORKER_ARGS+=(--dispatcher-id", source)

    def test_update_persists_before_ack_and_retries_after_transport_failure(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            state = worker._load_or_create_worker_state("hermes-agent", dispatcher_id="retry-dispatcher")
            update = update_fixture(worker, state)
            events: list[str] = []
            real_save = worker._save_worker_state

            def save_with_trace(value, *args, **kwargs):
                events.append("save")
                return real_save(value, *args, **kwargs)

            with mock.patch.object(worker, "_save_worker_state", side_effect=save_with_trace), mock.patch.object(
                worker, "_ack_worker_identity_update", side_effect=lambda *_args, **_kwargs: events.append("ack")
            ):
                worker._apply_worker_identity_update("http://127.0.0.1:8075", state, update)
            self.assertEqual(events, ["save", "ack", "save"])
            self.assertNotIn("pending_identity_update", state)

            reset = dict(state)
            reset["canonical_worker_id"] = ""
            reset["worker_identity_update_generation"] = 0
            reset["acknowledged_generation"] = 0
            state.clear()
            state.update(worker._save_worker_state(reset))
            save_failure_update = update_fixture(worker, state, canonical="hermes-agent-save-failure")
            with mock.patch.object(worker, "_save_worker_state", side_effect=RuntimeError("save")) as save_mock, mock.patch.object(worker, "_ack_worker_identity_update") as ack_mock:
                with self.assertRaisesRegex(RuntimeError, "save"):
                    worker._apply_worker_identity_update("http://127.0.0.1:8075", state, save_failure_update)
                save_mock.assert_called_once()
                ack_mock.assert_not_called()
            update = update_fixture(worker, state, canonical="hermes-agent-restart")
            with mock.patch.object(worker, "_ack_worker_identity_update", side_effect=RuntimeError("transport")):
                with self.assertRaisesRegex(RuntimeError, "transport"):
                    worker._apply_worker_identity_update("http://127.0.0.1:8075", state, update)
            state_path = worker._worker_dispatcher_state_path(Path(root), "retry-dispatcher")
            persisted = json.loads(state_path.read_text())
            self.assertIn("pending_identity_update", persisted)
            self.assertEqual(persisted["canonical_worker_id"], "hermes-agent-restart")
            worker._release_worker_state_lock(state_path)
            restarted_worker = load_task_worker()
            restarted = restarted_worker._load_or_create_worker_state("hermes-agent", dispatcher_id="retry-dispatcher")
            pending = restarted["pending_identity_update"]
            ack_update = {**pending, "state": "acknowledged", "ack_required": False}
            ack_response = {
                "schema_id": "agent_worker_identity_ack.v1",
                "contract_version": 1,
                "update_id": pending["update_id"],
                "identity_id": pending["identity_id"],
                "principal_id": pending["principal_id"],
                "workspace_id": pending["workspace_id"],
                "worker_instance_id": pending["worker_instance_id"],
                "old_worker_id": pending["old_worker_id"],
                "requested_worker_id": pending["requested_worker_id"],
                "canonical_worker_id": pending["canonical_worker_id"],
                "new_worker_id": pending["new_worker_id"],
                "worker_identity_update_generation": pending["worker_identity_update_generation"],
                "update_digest": pending["update_digest"],
                "receipt_digest": pending["receipt_digest"],
                "ack_receipt_digest": pending["ack_receipt_digest"],
                "acknowledged": True,
                "idempotent_replay": True,
                "identity_update": ack_update,
                "format_contract": {},
            }
            ack_response = restarted_worker.attach_format_contract("agent_worker_identity_ack.v1", ack_response)
            identity_response = registration_fixture(
                restarted_worker,
                restarted,
                generation=1,
                canonical="hermes-agent-restart",
                acknowledged=1,
            )
            with mock.patch.object(restarted_worker, "_post", side_effect=[identity_response, ack_response]):
                resumed = restarted_worker._register_worker_identity("http://127.0.0.1:8075", restarted)
            self.assertNotIn("pending_identity_update", resumed)
            self.assertEqual(resumed["worker_identity_update_generation"], 1)

    def test_registration_authority_rebind_fails_closed_without_state_mutation(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ), mock.patch.object(worker, "_worker_authority_config", return_value=("", "")):
            for update in (None, update_fixture(worker, {"worker_instance_id": "placeholder"})):
                state = worker._load_or_create_worker_state("hermes-agent")
                state["principal_id"] = "principal-persisted"
                state["workspace_id"] = "workspace-persisted"
                state["worker_instance_credential"] = "a" * 64
                state = worker._save_worker_state(state)
                before = dict(state)
                path = Path(root) / "worker_identity.json"
                before_json = path.read_text()
                response = registration_fixture(
                    worker,
                    state,
                    generation=1 if update is not None else 0,
                    canonical="hermes-agent-abc123" if update is not None else "hermes-agent",
                    acknowledged=0 if update is not None else None,
                    update=update,
                )
                with mock.patch.object(worker, "_post", return_value=response):
                    with self.assertRaisesRegex(RuntimeError, "does not match the persisted instance"):
                        worker._register_worker_identity("http://127.0.0.1:8075", state)
                self.assertEqual(state, before)
                self.assertEqual(path.read_text(), before_json)
                worker._release_worker_state_lock(path)

    def test_update_validation_rejects_unknown_or_tampered_fields(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            state = worker._load_or_create_worker_state("hermes-agent")
            update = update_fixture(worker, state)
            tampered = dict(update)
            tampered["canonical_worker_id"] = "forged"
            with self.assertRaisesRegex(RuntimeError, "invalid"):
                worker._worker_identity_update_from_response({"identity_update": tampered})
            unknown = dict(update)
            unknown["unexpected"] = True
            with self.assertRaisesRegex(RuntimeError, "invalid"):
                worker._worker_identity_update_from_response({"identity_update": unknown})

    def test_next_update_is_acknowledged_before_task_is_returned(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            state = worker._load_or_create_worker_state("hermes-agent")
            state["canonical_worker_id"] = "hermes-agent"
            state["principal_id"] = "principal-1"
            state["workspace_id"] = "workspace-1"
            worker._save_worker_state(state)
            update = update_fixture(worker, state)
            ack_receipt = worker._worker_identity_ack_receipt_digest(update)
            calls: list[tuple[str, dict[str, object]]] = []

            def fake_post(orchestrator_url, path, payload, params=None, *, timeout=30.0):
                calls.append((path, dict(payload)))
                if path.endswith("/next"):
                    return {"task": {"id": "must-not-cross"}, "identity_update": update, "identity_update_required": True}
                self.assertEqual(path, "/agents/workers/identity/ack")
                self.assertEqual(payload["ack_receipt_digest"], ack_receipt)
                self.assertEqual(payload["identity_update"], update)
                response = {
                    "schema_id": "agent_worker_identity_ack.v1",
                    "contract_version": 1,
                    "update_id": update["update_id"],
                    "identity_id": update["identity_id"],
                    "principal_id": update["principal_id"],
                    "workspace_id": update["workspace_id"],
                    "worker_instance_id": update["worker_instance_id"],
                    "old_worker_id": update["old_worker_id"],
                    "requested_worker_id": update["requested_worker_id"],
                    "canonical_worker_id": update["canonical_worker_id"],
                    "new_worker_id": update["new_worker_id"],
                    "worker_identity_update_generation": update["worker_identity_update_generation"],
                    "update_digest": update["update_digest"],
                    "receipt_digest": update["receipt_digest"],
                    "ack_receipt_digest": ack_receipt,
                    "acknowledged": True,
                    "idempotent_replay": False,
                    "identity_update": {**update, "state": "acknowledged", "ack_required": False, "ack_receipt_digest": ack_receipt},
                    "format_contract": {},
                }
                return worker.attach_format_contract("agent_worker_identity_ack.v1", response)

            with mock.patch.object(worker, "_post", side_effect=fake_post):
                result = worker._claim_next_task("http://127.0.0.1:8075", "hermes-agent", state=state)
            self.assertIsNone(result.get("task"))
            self.assertTrue(result.get("identity_update_acknowledged"))
            self.assertEqual(state["canonical_worker_id"], "hermes-agent-abc123")
            self.assertEqual(state["worker_identity_update_generation"], 1)
            self.assertEqual([path for path, _ in calls], ["/agents/tasks/next", "/agents/workers/identity/ack"])
            claim_payload = calls[0][1]
            self.assertEqual(claim_payload["worker"], "hermes-agent")
            self.assertEqual(claim_payload["requested_worker_id"], "hermes-agent")
            self.assertEqual(claim_payload["worker_instance_id"], state["worker_instance_id"])
            self.assertNotEqual(claim_payload["worker_instance_id"], claim_payload["requested_worker_id"])

            with mock.patch.object(worker, "_post", return_value={"task": None}) as next_post:
                worker._claim_next_task("http://127.0.0.1:8075", "hermes-agent", state=state)
            canonical_payload = next_post.call_args.args[2]
            self.assertEqual(canonical_payload["worker"], "hermes-agent-abc123")

    def test_registration_persists_server_canonical_without_instance_fallback(self) -> None:
        worker = load_task_worker()
        with tempfile.TemporaryDirectory() as root, mock.patch.dict(
            os.environ, {"TASK_AGENT_WORKER_STATE_ROOT": root}, clear=False
        ):
            state = worker._load_or_create_worker_state("hermes-agent")
            captured: dict[str, object] = {}

            def fake_post(orchestrator_url, path, payload, params=None, *, timeout=30.0):
                captured["path"] = path
                captured["payload"] = dict(payload)
                return registration_fixture(worker, state)

            with mock.patch.object(worker, "_post", side_effect=fake_post):
                registered = worker._register_worker_identity("http://127.0.0.1:8075", state)
            self.assertEqual(captured["path"], "/agents/workers/register")
            self.assertEqual(captured["payload"]["requested_worker_id"], "hermes-agent")
            self.assertEqual(captured["payload"]["worker_instance_id"], state["worker_instance_id"])
            self.assertEqual(registered["canonical_worker_id"], "hermes-agent")
            self.assertNotEqual(registered["worker_instance_id"], registered["requested_worker_id"])

    def test_forwarded_worker_errors_use_canonical_secret_and_path_redaction(self) -> None:
        worker = load_task_worker()
        raw = (
            "API_KEY=short-secret password=abc123 Authorization: Basic dXNlcjpwYXNz "
            "/Users/example/private.txt path:/Users/example/private2.txt"
        )
        redacted = worker._redact_runner_text(raw)
        for value in (
            "short-secret",
            "abc123",
            "dXNlcjpwYXNz",
            "/Users/example/private.txt",
            "/Users/example/private2.txt",
        ):
            self.assertNotIn(value, redacted)
        self.assertIn("[LOCAL_PATH]", redacted)

    def test_conflicting_claim_never_falls_through_to_legacy_execution(self) -> None:
        worker = load_task_worker()
        task = {"id": "task-u3", "task_id": "task-u3"}
        claim = {
            "task": task,
            "attempt": {"attempt_id": "attempt-u3"},
            "lease": {
                "task_id": "task-u3",
                "attempt_id": "attempt-u3",
                "lease_id": "lease-u3",
                "worker_id": "worker-u3",
                "worker_instance_id": "instance-u3",
                "generation": 1,
            },
            "fence": {"lease_id": "foreign-lease"},
        }
        with mock.patch.object(worker, "_strict_claimed_task") as strict:
            with self.assertRaises(worker.ExecutionBlocked) as blocked:
                worker._handle_task(
                    "http://gateway.invalid",
                    task,
                    "droid",
                    "disabled",
                    "disabled",
                    None,
                    None,
                    claim=claim,
                    worker="worker-u3",
                    worker_instance="instance-u3",
                )
        self.assertEqual(blocked.exception.reason, "conflicting_lease_fence")
        strict.assert_not_called()

    def test_legacy_adapter_outer_timeout_exists_only_when_explicitly_authorized(self) -> None:
        worker = load_task_worker()
        with mock.patch.dict(worker.os.environ, {"TASK_RUNNER_TIMEOUT_SECS": ""}, clear=False):
            self.assertIsNone(worker._runner_timeout_secs({}))
            self.assertEqual(worker._runner_timeout_secs({"max_runtime_secs": 12}), 42)
        with mock.patch.dict(worker.os.environ, {"TASK_RUNNER_TIMEOUT_SECS": "25"}, clear=False):
            self.assertEqual(worker._runner_timeout_secs({}), 25)

    def test_strict_gateway_inference_uses_exact_prepared_runtime_policy_timeout(self) -> None:
        worker = load_task_worker()
        claim = {
            "task": {"id": "task-u3", "title": "Bound provider call"},
            "attempt": {"attempt_id": "attempt-u3"},
            "lease": {
                "task_id": "task-u3",
                "attempt_id": "attempt-u3",
                "lease_id": "lease-u3",
                "worker_id": "worker-u3",
                "worker_instance_id": "instance-u3",
                "generation": 1,
            },
        }
        for runtime_secs in (900, 12):
            with self.subTest(runtime_secs=runtime_secs):
                calls: list[dict[str, object]] = []
                prepared = SimpleNamespace(
                    env={"TASK_PAYLOAD": "{}"},
                    fence=SimpleNamespace(task_id="task-u3"),
                    task={"title": "Bound provider call"},
                    prompt="bounded context",
                    profile={
                        "_runtime_policy": {
                            "effective_runtime_secs": runtime_secs,
                            "source": "registered_profile" if runtime_secs > 95 else "fenced_attempt",
                        }
                    },
                )

                lost = threading.Event()

                def fake_execute_claimed_task(*_args, **kwargs):
                    return kwargs["gateway_inference"](prepared, lost)

                def fake_post(
                    _base: str,
                    path: str,
                    payload: dict[str, object],
                    *_args,
                    **kwargs,
                ) -> dict[str, object]:
                    calls.append(
                        {
                            "path": path,
                            "payload": dict(payload),
                            "timeout": kwargs.get("timeout"),
                            "cancel_event": kwargs.get("cancel_event"),
                        }
                    )
                    return {"content": "bounded output", "route": {"provider": "local"}}

                with (
                    mock.patch.object(
                        worker,
                        "execute_claimed_task",
                        side_effect=fake_execute_claimed_task,
                    ),
                    mock.patch.object(worker, "_post", side_effect=fake_post),
                ):
                    output, route = worker._strict_claimed_task(
                        "http://gateway.invalid",
                        claim["task"],
                        claim,
                        "droid",
                        "local",
                        "model",
                        None,
                        "worker-u3",
                        "instance-u3",
                    )
                self.assertEqual(output, "bounded output")
                self.assertEqual(route["provider"], "local")
                self.assertEqual(len(calls), 1)
                self.assertEqual(calls[0]["path"], "/v1/inference/chat")
                self.assertEqual(calls[0]["timeout"], runtime_secs)
                self.assertEqual(calls[0]["payload"]["timeout_secs"], runtime_secs)  # type: ignore[index]
                self.assertIs(calls[0]["cancel_event"], lost)

    def test_strict_gateway_inference_lease_loss_closes_blocked_http_without_publish(self) -> None:
        worker = load_task_worker()
        claim = {
            "task": {"id": "task-u3", "title": "Cancel blocked inference"},
            "attempt": {"attempt_id": "attempt-u3"},
            "lease": {
                "task_id": "task-u3",
                "attempt_id": "attempt-u3",
                "lease_id": "lease-u3",
                "worker_id": "worker-u3",
                "worker_instance_id": "instance-u3",
                "generation": 1,
            },
        }

        for attempt in range(25):
            with self.subTest(attempt=attempt):
                request_started = threading.Event()
                gateway_disconnected = threading.Event()
                release_gateway = threading.Event()
                stop_server = threading.Event()
                server_state_lock = threading.Lock()
                paths: list[str] = []
                inference_request_ids: list[str] = []
                cancellation_request_ids: list[str] = []
                active_connections: set[socket.socket] = set()
                active_handler_threads: set[threading.Thread] = set()
                server_errors: list[str] = []

                class BlockingGatewayHandler(http.server.BaseHTTPRequestHandler):
                    protocol_version = "HTTP/1.1"

                    def log_message(self, *_args: object) -> None:
                        return None

                    def setup(self) -> None:
                        super().setup()
                        with server_state_lock:
                            active_connections.add(self.connection)
                            active_handler_threads.add(threading.current_thread())

                    def finish(self) -> None:
                        try:
                            super().finish()
                        finally:
                            with server_state_lock:
                                active_connections.discard(self.connection)
                                active_handler_threads.discard(threading.current_thread())

                    def handle(self) -> None:
                        try:
                            super().handle()
                        except OSError:
                            return

                    def do_POST(self) -> None:
                        with server_state_lock:
                            paths.append(self.path)
                        length = int(self.headers.get("content-length", "0"))
                        raw_body = self.rfile.read(length) if length else b"{}"
                        if self.path == "/v1/inference/chat":
                            inference_payload = json.loads(raw_body)
                            with server_state_lock:
                                inference_request_ids.append(inference_payload["request_id"])
                            request_started.set()
                            self.connection.settimeout(0.02)
                            while not release_gateway.is_set():
                                try:
                                    pending = self.connection.recv(1, socket.MSG_PEEK)
                                except TimeoutError:
                                    continue
                                except OSError:
                                    gateway_disconnected.set()
                                    return
                                if not pending:
                                    gateway_disconnected.set()
                                    return
                            self.close_connection = True
                            return
                        if self.path == "/v1/inference/cancel":
                            cancel_payload = json.loads(raw_body)
                            with server_state_lock:
                                cancellation_request_ids.append(cancel_payload["request_id"])
                        encoded = b'{"ok":true,"status":"running"}'
                        self.send_response(200)
                        self.send_header("content-type", "application/json")
                        self.send_header("content-length", str(len(encoded)))
                        self.send_header("connection", "close")
                        self.end_headers()
                        self.wfile.write(encoded)
                        self.close_connection = True

                class DeterministicThreadingHTTPServer(http.server.ThreadingHTTPServer):
                    daemon_threads = True
                    block_on_close = False

                    def handle_error(self, *_args: object) -> None:
                        with server_state_lock:
                            server_errors.append("handler_error")

                server = DeterministicThreadingHTTPServer(
                    ("127.0.0.1", 0),
                    BlockingGatewayHandler,
                )
                server.timeout = 0.02

                def serve_requests() -> None:
                    try:
                        while not stop_server.is_set():
                            server.handle_request()
                    except (OSError, ValueError):
                        if not stop_server.is_set():
                            with server_state_lock:
                                server_errors.append("listener_error")

                server_thread = threading.Thread(
                    target=serve_requests,
                    name=f"task-gateway-test-listener-{attempt}",
                    daemon=True,
                )
                server_thread_started = False
                listener_alive_after_cleanup = False
                handler_count_after_cleanup = 0
                try:
                    server_thread.start()
                    server_thread_started = True
                    base_url = f"http://127.0.0.1:{server.server_port}"
                    prepared = SimpleNamespace(
                        env={"TASK_PAYLOAD": "{}"},
                        fence=SimpleNamespace(task_id="task-u3"),
                        task={"title": "Cancel blocked inference"},
                        prompt="bounded context",
                        profile={"_runtime_policy": {"effective_runtime_secs": 900}},
                    )

                    def fake_execute_claimed_task(*_args: object, **kwargs: object):
                        lost = threading.Event()

                        def cancel_after_request_starts() -> None:
                            request_started.wait(0.25)
                            lost.set()

                        cancel_thread = threading.Thread(
                            target=cancel_after_request_starts,
                            name=f"task-gateway-test-cancel-{attempt}",
                            daemon=True,
                        )
                        cancel_thread.start()
                        try:
                            return kwargs["gateway_inference"](prepared, lost)  # type: ignore[index,operator]
                        finally:
                            cancel_thread.join(timeout=1.0)

                    started = time.monotonic()
                    with mock.patch.object(
                        worker,
                        "execute_claimed_task",
                        side_effect=fake_execute_claimed_task,
                    ):
                        result = worker._strict_claimed_task(
                            base_url,
                            claim["task"],
                            claim,
                            "droid",
                            "local",
                            "model",
                            None,
                            "worker-u3",
                            "instance-u3",
                        )
                    elapsed = time.monotonic() - started
                    with server_state_lock:
                        observed_paths = list(paths)
                        observed_inference_ids = list(inference_request_ids)
                        observed_cancellation_ids = list(cancellation_request_ids)
                    self.assertEqual(result["status"], "execution_failed")
                    self.assertEqual(result["reason"], "lease_lost")
                    self.assertTrue(result["execution_observed"])
                    self.assertTrue(request_started.is_set())
                    self.assertTrue(gateway_disconnected.wait(0.5))
                    self.assertFalse(any(path.endswith("/publish") for path in observed_paths))
                    self.assertTrue(any(path.endswith("/v1/inference/cancel") for path in observed_paths))
                    self.assertEqual(observed_cancellation_ids, observed_inference_ids)
                    self.assertEqual(len(observed_cancellation_ids[0]), 64)
                    self.assertTrue(any(path.endswith("/observe") for path in observed_paths))
                    self.assertLess(elapsed, 0.75)
                finally:
                    release_gateway.set()
                    stop_server.set()
                    server.server_close()
                    cleanup_deadline = time.monotonic() + 1.0
                    while time.monotonic() < cleanup_deadline:
                        with server_state_lock:
                            connections = list(active_connections)
                            handler_threads = list(active_handler_threads)
                        for connection in connections:
                            try:
                                connection.shutdown(socket.SHUT_RDWR)
                            except OSError:
                                pass
                            try:
                                connection.close()
                            except OSError:
                                pass
                        remaining = cleanup_deadline - time.monotonic()
                        if server_thread_started and remaining > 0:
                            server_thread.join(timeout=min(0.05, remaining))
                        for handler_thread in handler_threads:
                            remaining = cleanup_deadline - time.monotonic()
                            if remaining <= 0:
                                break
                            handler_thread.join(timeout=min(0.05, remaining))
                        with server_state_lock:
                            handler_count_after_cleanup = len(active_handler_threads)
                        listener_alive_after_cleanup = (
                            server_thread.is_alive() if server_thread_started else False
                        )
                        if not listener_alive_after_cleanup and handler_count_after_cleanup == 0:
                            break
                self.assertFalse(listener_alive_after_cleanup)
                self.assertEqual(handler_count_after_cleanup, 0)
                self.assertEqual(server_errors, [])

    def test_adapter_helper_stderr_and_compaction_outputs_share_one_redaction_boundary(self) -> None:
        worker = load_task_worker()
        digest = "sha256:" + "a" * 64
        workspace_ref = "workspace-" + "b" * 32
        raw_result = {
            "schema_id": "runner_result.v1",
            "ok": False,
            "runner": "droid",
            "status": "failed",
            "summary": "token=s",
            "stdout_tail": "API_KEY=out /opt/local/bin/tool",
            "stderr_tail": "password=err /var/local/error.log",
            "artifacts": [
                {
                    "artifact_digest": digest,
                    "workspace_ref": workspace_ref,
                    "findings": {"token": "f", "helper_output": "/Applications/Runner.app/runner"},
                }
            ],
            "warnings": ["token=w /home/local-user/.config/tool"],
            "metadata": {
                "token": "m",
                "helper_output": "/Users/example/private.txt",
                "artifact_digest": digest,
            },
        }
        parsed = worker._parse_adapter_result("droid", json.dumps(raw_result), "token=raw-stderr", 1)
        compact = worker._compact_runner_result(parsed)
        fallback = worker._fallback_runner_result(
            "droid",
            "failed",
            1,
            "token=fallback",
            stderr="token=raw-stderr /Volumes/example/private.txt",
        )
        quality = worker._compact_runner_quality_sample(
            {
                "schema_id": "runner_quality_sample.v1",
                "sample_id": "sample-1",
                "context_pack_quality": {"confidence": "token=quality /var/local/quality"},
                "token_impact": {"provider_total_tokens": 7},
                "outcome": {},
            },
            {"enabled": True, "sample_id": "token=storage /opt/local/storage"},
        )
        env_handoff = worker._serialize_env_json(
            {"token": "env", "artifact_digest": digest, "workspace_ref": workspace_ref, "helper": "/Applications/Runner.app"}
        )
        formatted = worker._format_result(
            {"id": "task-1", "project": "p", "agent": "droid", "payload": {"token": "payload", "digest": digest}},
            "token=output /opt/local/output",
        )
        serialized = json.dumps(
            {
                "parsed": parsed,
                "compact": compact,
                "fallback": fallback,
                "quality": quality,
                "env_handoff": env_handoff,
                "formatted": formatted,
            },
            sort_keys=True,
        )
        for leaked in (
            "token=s",
            "API_KEY=out",
            "password=err",
            "raw-stderr",
            "token=w",
            '"token": "m"',
            "/opt/local",
            "/var/local",
            "/Applications/Runner.app",
            "/home/local-user",
            "/Users/example",
            "/Volumes/example",
            "token=quality",
            "token=storage",
            '"token": "payload"',
            "token=output",
        ):
            self.assertNotIn(leaked, serialized)
        self.assertIn(digest, serialized)
        self.assertIn(workspace_ref, serialized)
        self.assertIn('"provider_total_tokens": 7', serialized)

    def test_timeout_bytes_and_nested_serialized_values_use_canonical_boundary(self) -> None:
        worker = load_task_worker()
        failure = subprocess.TimeoutExpired(
            ["runner"],
            3,
            output=b'API_KEY=bytes-secret {"password":"nested-secret"} C:\\Users\\private\\token.json',
            stderr=b"Authorization: Basic dXNlcjpwYXNz /var/private/error.log",
        )
        with mock.patch.object(worker.subprocess, "run", side_effect=failure):
            result = worker._run_adapter(
                ["runner"],
                {},
                REPO_ROOT,
                3,
                "droid",
            )
        serialized = json.dumps(result, sort_keys=True)
        for leaked in (
            "bytes-secret",
            "nested-secret",
            "dXNlcjpwYXNz",
            r"C:\Users\private",
            "/var/private",
        ):
            self.assertNotIn(leaked, serialized)
        self.assertIn("[REDACTED", serialized)

    def test_public_status_memory_feedback_quality_and_checkpoint_sinks_do_not_leak(self) -> None:
        worker = load_task_worker()
        digest = "sha256:" + "a" * 64
        raw_digest = "sha256:" + "b" * 64
        captured_posts: list[tuple[str, dict[str, object]]] = []

        def fake_post(_base: str, path: str, payload: dict[str, object], **_kwargs: object) -> dict[str, object]:
            captured_posts.append((path, payload))
            if path == "/telemetry/context-pack-quality/outcome":
                return {"ok": True, "recorded": True, "outcome": {"outcome_id": "outcome-1"}}
            return {"ok": True}

        class Runtime:
            def __init__(self) -> None:
                self.calls: list[dict[str, object]] = []

            def write_checkpoint(self, **kwargs: object) -> dict[str, object]:
                self.calls.append(kwargs)
                return {"ok": True}

        runtime = Runtime()
        quality_inputs: dict[str, object] = {}

        def fake_quality(**kwargs: object):
            quality_inputs.update(kwargs)
            return (
                {
                    "schema_id": "runner_quality_sample.v1",
                    "sample_id": "sample-1",
                    "context_pack_quality": {},
                    "token_impact": {},
                    "outcome": {},
                },
                {"enabled": True, "sample_id": "sample-1"},
            )

        unsafe = {
            "message": f"API_KEY=status-secret /Applications/Private.app {raw_digest}",
            "metadata": {
                "password": "metadata-secret",
                "artifact_digest": digest,
                "nested_json": json.dumps(
                    {"token": "nested-secret", "path": r"C:\Users\private\config.json"}
                ),
                r"\\server\share\private": "mapping-key",
            },
        }
        with (
            mock.patch.object(worker, "_post", side_effect=fake_post),
            mock.patch.object(worker, "record_runner_quality", side_effect=fake_quality),
        ):
            worker._post_status("http://gateway.invalid", "task-1", unsafe)
            worker._write_memory(
                "http://gateway.invalid",
                "/Users/private/project",
                "task_runs/task-1.md",
                "password=memory-secret /opt/private/result",
                topic_path="file:///var/private/topic",
            )
            worker._post_feedback(
                "http://gateway.invalid",
                {
                    "content": "token=feedback-secret",
                    "topic_path": r"C:\Users\private\topic",
                    "metadata": {
                        "base_url": "https://user:pass@private.invalid/v1?api_key=query-secret",
                        "artifact_digest": digest,
                    },
                },
            )
            worker._write_checkpoint(
                runtime,
                task={"id": "task-1", "payload": {"password": "task-secret"}},
                bundle={"warning": b"token=bundle-secret /Volumes/private"},
                output="Authorization: Bearer checkpoint-secret /home/private",
                provider="provider",
                model="model",
                status="failed",
            )
            worker._record_runner_quality_sample(
                task={"id": "task-1", "payload": {"token": "quality-task-secret"}},
                agent="droid",
                result={"metadata": {"password": "quality-result-secret"}},
                context_bundle={"warning": "/Users/private/quality"},
                task_status="failed",
                message="API_KEY=quality-message-secret",
                route_payload={
                    "base_url": "https://user:pass@private.invalid/v1?token=route-secret",
                    "artifact_digest": digest,
                },
            )
            worker._post_context_pack_outcome(
                "http://gateway.invalid",
                task={"id": "task-1", "project": "/Users/private/project", "payload": {}},
                context_bundle={"context_pack_quality": {"sample_id": "sample-1"}},
                status="failed",
                source="runner_adapter",
                calibration_eligible=False,
                outcome_class="infrastructure_failure",
                result_metadata={"policy_id": "password=outcome-secret"},
            )

        serialized = json.dumps(
            {
                "posts": captured_posts,
                "checkpoints": runtime.calls,
                "quality_inputs": quality_inputs,
            },
            sort_keys=True,
        )
        for leaked in (
            "status-secret",
            "metadata-secret",
            "nested-secret",
            "memory-secret",
            "feedback-secret",
            "query-secret",
            "checkpoint-secret",
            "bundle-secret",
            "task-secret",
            "quality-task-secret",
            "quality-result-secret",
            "quality-message-secret",
            "route-secret",
            "outcome-secret",
            "user:pass",
            "/Applications/Private.app",
            "/Users/private",
            "/opt/private",
            "/var/private",
            "/home/private",
            "/Volumes/private",
            r"C:\Users\private",
            r"\\server\share\private",
            raw_digest,
        ):
            self.assertNotIn(leaked, serialized)
        self.assertIn(digest, serialized)


if __name__ == "__main__":
    unittest.main()
