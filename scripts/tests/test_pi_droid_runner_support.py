#!/usr/bin/env python3
"""Focused tests for optional Pi/Droid runner support."""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest
from contextlib import ExitStack
from pathlib import Path
from typing import Any
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPTS_DIR = REPO_ROOT / "scripts"
sys.path.insert(0, str(SCRIPTS_DIR))
RUNNERS_DIR = REPO_ROOT / "scripts" / "agent_runners"
sys.path.insert(0, str(RUNNERS_DIR))

from agent_contracts import attach_format_contract, load_agent_contracts_registry, validate_agent_contract_payload  # noqa: E402
import context_expansion_runtime  # noqa: E402
import runner_quality  # noqa: E402


def load_task_worker():
    path = REPO_ROOT / "scripts" / "task_agent_worker.py"
    spec = importlib.util.spec_from_file_location("task_agent_worker_under_test", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("failed to load task_agent_worker.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class EnvPatch:
    def __init__(self, values: dict[str, str | None]) -> None:
        self.values = values
        self.original: dict[str, str | None] = {}

    def __enter__(self) -> None:
        for key, value in self.values.items():
            self.original[key] = os.environ.get(key)
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value

    def __exit__(self, *exc: object) -> None:
        for key, value in self.original.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


def adapter_env(payload: str = "{}") -> dict[str, str]:
    env = os.environ.copy()
    env.update(
        {
            "TASK_ID": "task_test",
            "TASK_TITLE": "Runner test",
            "TASK_PROJECT": "contextlattice",
            "TASK_PAYLOAD": payload,
            "TASK_AGENT": "pi",
            "PATH": "/usr/bin:/bin",
        }
    )
    for key in ("PI_CODING_AGENT_BIN", "PI_BIN", "DROID_BIN"):
        env.pop(key, None)
    return env


class PiDroidRunnerSupportTests(unittest.TestCase):
    def setUp(self) -> None:
        self._runner_quality_dir = tempfile.TemporaryDirectory()
        self.addCleanup(self._runner_quality_dir.cleanup)
        self._runner_quality_env = EnvPatch(
            {
                "CONTEXTLATTICE_RUNNER_QUALITY_LEDGER_PATH": str(
                    Path(self._runner_quality_dir.name) / "runner_quality.ndjson"
                )
            }
        )
        self._runner_quality_env.__enter__()
        self.addCleanup(self._runner_quality_env.__exit__)

    def test_profiles_and_contracts_present(self) -> None:
        profiles = json.loads((REPO_ROOT / "config" / "agents" / "agent_profiles.json").read_text())
        self.assertEqual(profiles["profiles"]["pi"]["agent_id"], "pi_agent")
        self.assertEqual(profiles["profiles"]["droid"]["agent_id"], "droid_agent")
        self.assertEqual(profiles["profiles"]["omp"]["agent_id"], "omp_agent")
        self.assertEqual(profiles["profiles"]["mercury-agent"]["agent_id"], "mercury_agent")
        self.assertEqual(profiles["profiles"]["pi"]["state_authority"], "manual")
        self.assertEqual(profiles["profiles"]["droid"]["state_authority"], "manual")
        self.assertEqual(profiles["profiles"]["omp"]["state_authority"], "self_report")
        self.assertEqual(profiles["profiles"]["mercury-agent"]["state_authority"], "self_report")
        self.assertIn("agent_state", profiles["adapter_contract"]["required_phases"])
        self.assertIn("pi-coding-agent", profiles["profiles"]["pi"]["surfaces"])
        self.assertIn("brew-cask", profiles["profiles"]["droid"]["surfaces"])
        self.assertIn("oh-my-pi", profiles["profiles"]["omp"]["surfaces"])
        self.assertIn("mercury", profiles["profiles"]["mercury-agent"]["process_names"])

        registry = load_agent_contracts_registry()
        for contract_id in ("runner_capability.v1", "runner_result.v1", "agent_task_lease.v1", "runner_quality_sample.v1"):
            self.assertIn(contract_id, registry["contracts"])

    def test_contract_examples_validate(self) -> None:
        registry = load_agent_contracts_registry()
        pi_capability = attach_format_contract(
            "runner_capability.v1",
            {
                "schema_id": "runner_capability.v1",
                "runner": "pi",
                "agent": "pi",
                "agent_id": "pi_agent",
                "roles": ["scout", "summarizer", "reviewer", "light_refactor"],
                "surfaces": ["cli", "json", "brew"],
                "install": {"brew": "pi-coding-agent", "brew_cask": "", "hint": "brew install pi-coding-agent"},
                "detection": {"commands": ["pi"], "env_overrides": ["PI_CODING_AGENT_BIN", "PI_BIN"]},
                "execution": {
                    "adapter": "scripts/agent_runners/pi_runner.py",
                    "supports_worktree": False,
                    "supports_json_output": True,
                    "requires_explicit_cwd": True,
                },
                "safety": {
                    "default_write_access": False,
                    "requires_sandbox": True,
                    "no_auto_merge": True,
                    "no_git_push": True,
                    "redact_secrets": True,
                },
                "max_parallel": 3,
            },
            registry,
        )
        droid_capability = attach_format_contract(
            "runner_capability.v1",
            {
                **pi_capability,
                "runner": "droid",
                "agent": "droid",
                "agent_id": "droid_agent",
                "roles": ["scout", "reviewer", "implementer"],
                "surfaces": ["cli", "json", "brew-cask"],
                "install": {"brew": "", "brew_cask": "droid", "hint": "brew install --cask droid"},
                "detection": {"commands": ["droid"], "env_overrides": ["DROID_BIN"]},
                "execution": {
                    "adapter": "scripts/agent_runners/droid_runner.py",
                    "supports_worktree": True,
                    "supports_json_output": True,
                    "requires_explicit_cwd": True,
                },
                "max_parallel": 1,
            },
            registry,
        )
        runner_result = attach_format_contract(
            "runner_result.v1",
            {
                "schema_id": "runner_result.v1",
                "ok": False,
                "runner": "pi",
                "agent": "pi",
                "agent_id": "pi_agent",
                "task_id": "task_test",
                "project": "contextlattice",
                "status": "missing_binary",
                "exit_code": 127,
                "started_at": "2026-07-04T00:00:00Z",
                "completed_at": "2026-07-04T00:00:01Z",
                "duration_secs": 1.0,
                "summary": "pi binary not found; install with: brew install pi-coding-agent",
                "stdout_tail": "",
                "stderr_tail": "",
                "artifacts": [],
                "warnings": [],
                "metadata": {"install_hint": "brew install pi-coding-agent"},
            },
            registry,
        )
        lease = attach_format_contract(
            "agent_task_lease.v1",
            {
                "schema_id": "agent_task_lease.v1",
                "task_id": "task_test",
                "lease_id": "lease_test",
                "worker": "local-pi-01",
                "runner": "pi",
                "status": "claimed",
                "claimed_at": "2026-07-04T00:00:00Z",
                "expires_at": "2026-07-04T00:10:00Z",
                "heartbeat_required": False,
                "heartbeat_interval_secs": 0,
                "worktree": "",
                "cwd": "",
                "allowed_paths": [],
                "max_runtime_secs": 600,
                "capabilities": ["scout"],
                "metadata": {},
            },
            registry,
        )
        quality_sample = attach_format_contract(
            "runner_quality_sample.v1",
            {
                "schema_id": "runner_quality_sample.v1",
                "sample_id": "rqs_test",
                "captured_at": "2026-07-05T00:00:00Z",
                "runner": "pi",
                "agent": "pi",
                "agent_id": "pi_agent",
                "task_id": "task_test",
                "project": "contextlattice",
                "task_class": "scout",
                "status": "succeeded",
                "ok": True,
                "exit_code": 0,
                "duration_secs": 1.25,
                "context_pack_quality": {
                    "sample_id": "cpq_test",
                    "quality_score": 88,
                    "confidence": "medium",
                    "calibration_grade": "observed_outcome",
                    "exact_prompt_tokens_saved": 900,
                    "modeled_inference_tokens_avoided": 2400,
                    "modeled_extra_calls_avoided": 0.3,
                    "tokenizer_exact": True,
                },
                "token_impact": {
                    "saved_tokens_estimate": 900,
                    "packed_tokens_estimate": 600,
                    "tokenizer_exact": True,
                    "provider_prompt_tokens": 0,
                    "provider_completion_tokens": 0,
                    "provider_total_tokens": 0,
                },
                "outcome": {
                    "task_status": "succeeded",
                    "runner_status": "succeeded",
                    "first_pass_success": True,
                    "blocked": False,
                    "failed": False,
                    "retry_count": 0,
                    "observed_followup_tokens": 0,
                },
                "feedback": {"present": False, "rating": 0, "label": "", "source": ""},
                "metadata": {
                    "adapter": "pi_runner",
                    "context_pack_quality_sample_id": "cpq_test",
                    "summary_hash": "abc123",
                    "warning_count": 0,
                    "artifact_count": 0,
                    "retrieval_status": "completed",
                    "quality_basis": "context_pack_quality_sample",
                },
            },
            registry,
        )
        for contract_id, payload in (
            ("runner_capability.v1", pi_capability),
            ("runner_capability.v1", droid_capability),
            ("runner_result.v1", runner_result),
            ("agent_task_lease.v1", lease),
            ("runner_quality_sample.v1", quality_sample),
        ):
            self.assertEqual(validate_agent_contract_payload(contract_id, payload, registry), [])

    def test_task_worker_aliases_and_adapter_paths(self) -> None:
        worker = load_task_worker()
        self.assertEqual(worker._normalize_agent_alias("pi-coding-agent"), "pi")
        self.assertEqual(worker._normalize_agent_alias("factory-droid"), "droid")
        self.assertIn("pi_runner.py", worker._runner_adapter_for_agent("pi-coding-agent")[-1])
        self.assertIn("droid_runner.py", worker._runner_adapter_for_agent("factory-droid")[-1])
        with EnvPatch({"TASK_AGENT_CMD": "echo legacy"}):
            self.assertEqual(worker._runner_cmd_for_agent("pi"), "echo legacy")

    def test_governed_agent_fit_selection_authorizes_explicit_runner(self) -> None:
        worker = load_task_worker()
        task = {
            "id": "task-governed",
            "title": "Use the explicit runner",
            "project": "contextlattice",
            "agent": "droid",
            "approved": True,
            "payload": {
                "agent_fit_selection_authorize": True,
                "agent_fit_selection_expected_generation": 4,
                "agent_fit_selection_receipt": {
                    "receipt_id": "selection-receipt-1",
                    "task_id": "task-governed",
                    "kind": "runner",
                    "selected_id": "droid",
                    "sample_count": 8,
                    "decision": "selected",
                    "advisory_only": True,
                    "execution_performed": False,
                    "evidence_digest": "sha256:" + "a" * 64,
                    "expires_at": "2026-07-19T08:00:00Z",
                },
            },
        }
        captured: dict[str, Any] = {}

        def fake_post(
            _url: str,
            path: str,
            payload: dict[str, Any],
            *args: Any,
            **kwargs: Any,
        ) -> dict[str, Any]:
            captured["path"] = path
            captured["payload"] = payload
            return {
                "ok": True,
                "schema_id": "frontier_t6_agent_fit_governance.v1",
                "feature_id": "frontier_agent_fit_governance",
                "operation": "authorize",
                "result": {
                    "activation_id": "ft6ga_test",
                    "activation_owner": "external_task_worker",
                    "activation_delivery": "explicit_pull",
                    "kind": "runner",
                    "selected_id_digest": worker._agent_fit_opaque_digest(
                        "frontier-t6-governance-selected", "droid"
                    ),
                    "task_digest": worker._agent_fit_opaque_digest(
                        "frontier-t6-governance-task", "task-governed"
                    ),
                    "execution_performed": False,
                },
                "access": {"workspace_project_binding_verified": True},
                "safety": {
                    "gateway_execution_performed": False,
                    "model_execution_performed": False,
                    "subprocess_execution_performed": False,
                    "prompt_injection_performed": False,
                    "ordinary_memory_mutated": False,
                    "network_calls": 0,
                    "dispatch_owner": "external_task_worker",
                    "dispatch_mode": "explicit_pull",
                },
                "receipt": {
                    "receipt_id": "ft6gr_test",
                    "receipt_hash": "sha256:" + "b" * 64,
                    "policy_generation": 4,
                },
                "format_contract": {
                    "schema_id": "frontier_t6_agent_fit_governance.v1",
                    "validation": {"status": "passed"},
                },
            }

        with mock.patch.object(worker, "_post", side_effect=fake_post):
            result = worker._authorize_agent_fit_selection(
                "http://127.0.0.1:8075", task, "droid", "model-explicit"
            )

        self.assertEqual(captured["path"], "/memory/agent-fit/selection/activation")
        self.assertEqual(captured["payload"]["operation"], "authorize")
        self.assertEqual(captured["payload"]["selection_receipt"]["selected_id"], "droid")
        self.assertTrue(result["authorized"])
        self.assertEqual(result["selected_id"], "droid")
        self.assertFalse(result["execution_performed"])

    def test_governed_agent_fit_mismatch_blocks_before_execution(self) -> None:
        worker = load_task_worker()
        posts: list[tuple[str, dict[str, Any]]] = []

        def fake_post(
            _url: str,
            path: str,
            payload: dict[str, Any],
            *args: Any,
            **kwargs: Any,
        ) -> dict[str, Any]:
            posts.append((path, payload))
            return {"ok": True}

        blocked_surfaces = (
            "_runner_adapter_for_agent",
            "ContextExpansionRuntime",
            "_run_llm_task_via_gateway",
            "_run_adapter",
            "_run_command",
            "_write_memory",
            "_post_context_pack_outcome",
            "_post_feedback",
        )
        task = {
            "id": "task-mismatch",
            "title": "Reject hidden rerouting",
            "project": "contextlattice",
            "agent": "pi",
            "approved": True,
            "payload": {
                "agent_fit_selection_authorize": True,
                "agent_fit_selection_expected_generation": 1,
                "agent_fit_selection_receipt": {
                    "receipt_id": "selection-receipt-mismatch",
                    "task_id": "task-mismatch",
                    "kind": "runner",
                    "selected_id": "droid",
                },
            },
        }
        with ExitStack() as stack:
            surface_mocks = {
                name: stack.enter_context(mock.patch.object(worker, name))
                for name in blocked_surfaces
            }
            stack.enter_context(mock.patch.object(worker, "_post", side_effect=fake_post))
            worker._handle_task(
                "http://127.0.0.1:8075",
                task,
                "pi",
                "auto",
                "model-explicit",
                None,
                None,
            )

        for name, surface_mock in surface_mocks.items():
            with self.subTest(surface=name):
                surface_mock.assert_not_called()
        self.assertEqual(len(posts), 1)
        self.assertEqual(posts[0][0], "/agents/tasks/task-mismatch/status")
        self.assertEqual(posts[0][1]["status"], "blocked")
        self.assertIn("does not match", posts[0][1]["metadata"]["agent_fit_selection"]["reason"])

    def test_governed_agent_fit_respects_legacy_command_override(self) -> None:
        worker = load_task_worker()
        task = {
            "id": "task-legacy-governed",
            "project": "contextlattice",
            "agent": "pi",
            "approved": True,
            "payload": {
                "agent_fit_selection_authorize": True,
                "agent_fit_selection_expected_generation": 2,
                "agent_fit_selection_receipt": {
                    "receipt_id": "selection-receipt-legacy",
                    "task_id": "task-legacy-governed",
                    "kind": "runner",
                    "selected_id": "pi",
                },
            },
        }
        with EnvPatch({"TASK_AGENT_CMD": "echo legacy"}):
            with mock.patch.object(worker, "_post") as post:
                result = worker._authorize_agent_fit_selection(
                    "http://127.0.0.1:8075", task, "pi", "model-explicit"
                )
        post.assert_not_called()
        self.assertTrue(result["requested"])
        self.assertFalse(result["authorized"])
        self.assertEqual(result["reason"], "explicit_task_agent_cmd_override")

    def test_unapproved_task_blocks_before_any_work(self) -> None:
        worker = load_task_worker()
        posts: list[tuple[str, dict[str, Any]]] = []

        def fake_post(_url: str, path: str, payload: dict[str, Any], *args: Any, **kwargs: Any) -> dict[str, Any]:
            posts.append((path, payload))
            return {"ok": True}

        blocked_surfaces = (
            "_runner_adapter_for_agent",
            "ContextExpansionRuntime",
            "_run_llm_task_via_gateway",
            "_run_adapter",
            "_run_command",
            "_write_memory",
            "_post_context_pack_outcome",
            "_post_feedback",
        )
        with ExitStack() as stack:
            surface_mocks = {
                name: stack.enter_context(mock.patch.object(worker, name))
                for name in blocked_surfaces
            }
            stack.enter_context(mock.patch.object(worker, "_post", side_effect=fake_post))
            worker._handle_task(
                "http://127.0.0.1:8075",
                {"id": "task1", "title": "Needs approval", "project": "contextlattice", "agent": "pi", "payload": {}, "approval_required": True},
                "pi",
                "auto",
                "model",
                None,
                None,
            )
        for name, surface_mock in surface_mocks.items():
            with self.subTest(surface=name):
                surface_mock.assert_not_called()
        self.assertEqual(
            posts,
            [
                (
                    "/agents/tasks/task1/status",
                    {"status": "blocked", "message": "Awaiting approval"},
                )
            ],
        )

    def test_context_expansion_fail_open_still_runs_adapter(self) -> None:
        worker = load_task_worker()
        captured: dict[str, Any] = {}

        class FakeRuntime:
            def __init__(self, *_: Any, **__: Any) -> None:
                pass

            def prepare(self, task: dict[str, Any]) -> dict[str, Any]:
                raise RuntimeError("boom")

            def render_for_prompt(self, bundle: dict[str, Any]) -> str:
                return "unused"

            def write_checkpoint(self, **kwargs: Any) -> None:
                captured["checkpoint"] = kwargs

        def fake_post(_url: str, path: str, payload: dict[str, Any], *args: Any, **kwargs: Any) -> dict[str, Any]:
            if path == "/v1/inference/route":
                return {"route": {"provider": "test", "base_url": "", "reason": "test"}}
            captured.setdefault("posts", []).append((path, payload))
            return {"ok": True}

        def fake_run_adapter(_argv: list[str], env: dict[str, str], *_: Any, **__: Any) -> dict[str, Any]:
            captured["env"] = env
            return {
                "schema_id": "runner_result.v1",
                "ok": True,
                "runner": "pi",
                "agent": "pi",
                "agent_id": "pi_agent",
                "task_id": "task2",
                "project": "contextlattice",
                "status": "succeeded",
                "exit_code": 0,
                "summary": "ok",
                "stdout_tail": "",
                "stderr_tail": "",
                "artifacts": [],
                "warnings": [],
                "metadata": {"adapter": "pi_runner"},
            }

        original_runtime, original_post, original_run = worker.ContextExpansionRuntime, worker._post, worker._run_adapter
        try:
            worker.ContextExpansionRuntime = FakeRuntime
            worker._post = fake_post
            worker._run_adapter = fake_run_adapter
            worker._handle_task(
                "http://127.0.0.1:8075",
                {"id": "task2", "title": "Run", "project": "contextlattice", "agent": "pi", "payload": {}},
                "pi",
                "auto",
                "model",
                None,
                None,
            )
        finally:
            worker.ContextExpansionRuntime, worker._post, worker._run_adapter = original_runtime, original_post, original_run
        self.assertIn("Context expansion unavailable", captured["env"]["TASK_CONTEXT_PROMPT"])
        self.assertEqual(captured["posts"][0][1]["status"], "succeeded")
        self.assertIn("runner_result", captured["checkpoint"]["output"])

    def test_context_expansion_preserves_context_pack_quality_sample(self) -> None:
        class FakeRuntime(context_expansion_runtime.ContextExpansionRuntime):
            def _context_pack(self, **_: Any) -> dict[str, Any]:
                return {
                    "context_pack": {
                        "facts": [{"text": "fact"}],
                        "results": [],
                        "numericFacts": [],
                    },
                    "context_pack_quality": {
                        "sample_id": "cpq_preserved",
                        "quality_score": 91,
                        "confidence": "medium",
                        "exact_prompt_tokens_saved": 111,
                        "modeled_inference_tokens_avoided": 222,
                    },
                    "token_impact": {
                        "saved_tokens_estimate": 111,
                        "packed_tokens_estimate": 40,
                        "tokenizer_exact": True,
                    },
                }

            def _search(self, **_: Any) -> dict[str, Any]:
                return {
                    "results": [],
                    "grounding": {"facts": [{"text": "search fact"}], "numeric_facts": []},
                    "retrieval_lifecycle": {"status": "completed", "result_state": "ready"},
                    "source_summary": {"returned_now": ["postgres_pgvector"]},
                }

        runtime = FakeRuntime("http://127.0.0.1:8075")
        bundle = runtime.prepare({"id": "task_quality", "title": "Quality", "project": "contextlattice", "payload": {}})
        self.assertEqual(bundle["context_pack_quality"]["sample_id"], "cpq_preserved")
        self.assertEqual(bundle["token_impact"]["saved_tokens_estimate"], 111)

    def test_task_worker_records_runner_quality_sample(self) -> None:
        worker = load_task_worker()
        captured: dict[str, Any] = {}
        with tempfile.TemporaryDirectory() as tmp:
            ledger_path = Path(tmp) / "runner_quality.ndjson"

            class FakeRuntime:
                def __init__(self, *_: Any, **__: Any) -> None:
                    pass

                def prepare(self, task: dict[str, Any]) -> dict[str, Any]:
                    return {
                        "lifecycle": {"status": "completed", "result_state": "ready", "degraded": False},
                        "tool_slices": {},
                        "expansion": {},
                        "context_pack_quality": {
                            "sample_id": "cpq_worker",
                            "quality_score": 86,
                            "confidence": "medium",
                            "calibration_grade": "observed_outcome",
                            "exact_prompt_tokens_saved": 1234,
                            "modeled_inference_tokens_avoided": 2400,
                            "modeled_extra_calls_avoided": 0.25,
                            "tokenizer_exact": True,
                        },
                        "token_impact": {
                            "saved_tokens_estimate": 1234,
                            "packed_tokens_estimate": 900,
                            "tokenizer_exact": True,
                        },
                    }

                def render_for_prompt(self, bundle: dict[str, Any]) -> str:
                    return "context"

                def write_checkpoint(self, **kwargs: Any) -> None:
                    captured["checkpoint"] = kwargs

            def fake_post(_url: str, path: str, payload: dict[str, Any], *args: Any, **kwargs: Any) -> dict[str, Any]:
                if path == "/v1/inference/route":
                    return {"route": {"provider": "test", "base_url": "", "reason": "test"}}
                captured.setdefault("posts", []).append((path, payload))
                if path == "/telemetry/context-pack-quality/outcome":
                    return {
                        "ok": True,
                        "recorded": True,
                        "outcome": {
                            **payload,
                            "outcome_id": "cpo_task_quality",
                        },
                    }
                return {"ok": True}

            def fake_run_adapter(_argv: list[str], env: dict[str, str], *_: Any, **__: Any) -> dict[str, Any]:
                return {
                    "schema_id": "runner_result.v1",
                    "ok": True,
                    "runner": "pi",
                    "agent": "pi",
                    "agent_id": "pi_agent",
                    "task_id": "task_quality",
                    "project": "contextlattice",
                    "status": "succeeded",
                    "exit_code": 0,
                    "duration_secs": 1.2,
                    "summary": "finished with sk-abcdefghijklmnopqrstuvwxyz1234567890",
                    "stdout_tail": "Bearer abcdefghijklmnopqrstuvwxyz1234567890",
                    "stderr_tail": "",
                    "artifacts": [],
                    "warnings": [],
                    "metadata": {"adapter": "pi_runner"},
                }

            original_runtime, original_post, original_run = worker.ContextExpansionRuntime, worker._post, worker._run_adapter
            with EnvPatch({"CONTEXTLATTICE_RUNNER_QUALITY_LEDGER_PATH": str(ledger_path)}):
                try:
                    worker.ContextExpansionRuntime = FakeRuntime
                    worker._post = fake_post
                    worker._run_adapter = fake_run_adapter
                    worker._handle_task(
                        "http://127.0.0.1:8075",
                        {
                            "id": "task_quality",
                            "title": "Run",
                            "project": "contextlattice",
                            "agent": "pi",
                            "payload": {"policy_id": "ctxpol_test", "policy_arm": "canary", "policy_phase": "canary"},
                        },
                        "pi",
                        "auto",
                        "model",
                        None,
                        None,
                    )
                finally:
                    worker.ContextExpansionRuntime, worker._post, worker._run_adapter = original_runtime, original_post, original_run

            raw = ledger_path.read_text(encoding="utf-8")
            self.assertNotIn("sk-abcdefghijklmnopqrstuvwxyz", raw)
            self.assertNotIn("Bearer abcdef", raw)
            row = json.loads(raw.splitlines()[0])
            self.assertEqual(row["schema_id"], "runner_quality_sample.v1")
            self.assertEqual(row["task_class"], "general")
            self.assertEqual(row["context_pack_quality"]["sample_id"], "cpq_worker")
            self.assertEqual(row["context_pack_quality"]["exact_prompt_tokens_saved"], 1234)
            outcome_posts = [payload for path, payload in captured["posts"] if path == "/telemetry/context-pack-quality/outcome"]
            self.assertEqual(len(outcome_posts), 1)
            self.assertEqual(outcome_posts[0]["sample_id"], "cpq_worker")
            self.assertTrue(outcome_posts[0]["first_pass_success"])
            self.assertTrue(outcome_posts[0]["calibration_eligible"])
            self.assertEqual(outcome_posts[0]["policy_id"], "ctxpol_test")
            self.assertEqual(outcome_posts[0]["policy_arm"], "canary")
            self.assertEqual(outcome_posts[0]["policy_phase"], "canary")
            status_posts = [payload for path, payload in captured["posts"] if path.endswith("/status")]
            self.assertEqual(len(status_posts), 1)
            status_metadata = status_posts[0]["metadata"]
            self.assertEqual(status_metadata["runner_quality"]["context_pack_quality"]["sample_id"], "cpq_worker")
            self.assertEqual(status_metadata["context_pack_outcome"]["outcome_id"], "cpo_task_quality")
            summary = runner_quality.summarize([row])
            self.assertEqual(summary["by_runner"]["pi"]["success_count"], 1)
            self.assertEqual(summary["by_runner"]["pi"]["exact_prompt_tokens_saved"], 1234)
            self.assertEqual(summary["recommendations"]["mode"], "advisor_only")
            self.assertNotIn("routing_hint", summary)

    def test_context_pack_outcome_skips_when_context_sample_is_missing(self) -> None:
        worker = load_task_worker()
        result = worker._post_context_pack_outcome(
            "http://127.0.0.1:1",
            task={"id": "task_missing", "payload": {}},
            context_bundle={},
            status="succeeded",
            source="test",
            calibration_eligible=True,
            outcome_class="success",
        )
        self.assertFalse(result["ok"])
        self.assertEqual(result["reason"], "context_pack_quality_sample_missing")

    def test_legacy_task_agent_cmd_override_still_uses_legacy_command(self) -> None:
        worker = load_task_worker()
        called = {"legacy": False}

        class FakeRuntime:
            def __init__(self, *_: Any, **__: Any) -> None:
                pass

            def prepare(self, task: dict[str, Any]) -> dict[str, Any]:
                return {"lifecycle": {}, "tool_slices": {}, "expansion": {}}

            def render_for_prompt(self, bundle: dict[str, Any]) -> str:
                return "context"

            def write_checkpoint(self, **_: Any) -> None:
                pass

        def fake_post(_url: str, path: str, payload: dict[str, Any], *args: Any, **kwargs: Any) -> dict[str, Any]:
            if path == "/v1/inference/route":
                return {"route": {"provider": "test", "base_url": "", "reason": "test"}}
            return {"ok": True}

        def fake_run_command(cmd: str, env: dict[str, str]) -> int:
            called["legacy"] = cmd == "echo legacy"
            return 0

        original_runtime, original_post, original_run_command = worker.ContextExpansionRuntime, worker._post, worker._run_command
        with EnvPatch({"TASK_AGENT_CMD": "echo legacy"}):
            try:
                worker.ContextExpansionRuntime = FakeRuntime
                worker._post = fake_post
                worker._run_command = fake_run_command
                worker._handle_task(
                    "http://127.0.0.1:8075",
                    {"id": "task3", "title": "Legacy", "project": "contextlattice", "agent": "pi", "payload": {}},
                    "pi",
                    "auto",
                    "model",
                    None,
                    None,
                )
            finally:
                worker.ContextExpansionRuntime, worker._post, worker._run_command = original_runtime, original_post, original_run_command
        self.assertTrue(called["legacy"])

    def test_adapters_missing_binary_and_invalid_payload(self) -> None:
        for script, runner, hint in (
            ("pi_runner.py", "pi", "brew install pi-coding-agent"),
            ("droid_runner.py", "droid", "brew install --cask droid"),
        ):
            env = adapter_env("{}")
            env["PATH"] = "/usr/bin:/bin"
            proc = subprocess.run(
                [sys.executable, str(REPO_ROOT / "scripts" / "agent_runners" / script)],
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            payload = json.loads(proc.stdout)
            self.assertEqual(proc.returncode, 127)
            self.assertEqual(payload["runner"], runner)
            self.assertEqual(payload["status"], "missing_binary")
            self.assertIn(hint, payload["summary"])

        env = adapter_env("{not-json")
        proc = subprocess.run(
            [sys.executable, str(REPO_ROOT / "scripts" / "agent_runners" / "pi_runner.py")],
            env=env,
            text=True,
            capture_output=True,
            check=False,
        )
        payload = json.loads(proc.stdout)
        self.assertEqual(proc.returncode, 2)
        self.assertEqual(payload["status"], "invalid_task")

    def test_adapter_redacts_secrets_and_bounds_tails(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            fake = Path(tmp) / "fake_runner.py"
            fake.write_text(
                "#!/usr/bin/env python3\n"
                "import sys\n"
                "print('Bearer abcdefghijklmnopqrstuvwxyz1234567890 ' + 'x' * 5000)\n"
                "print('sk-abcdefghijklmnopqrstuvwxyz1234567890', file=sys.stderr)\n",
                encoding="utf-8",
            )
            fake.chmod(0o755)
            env = adapter_env(json.dumps({"cwd": tmp}))
            env["PI_CODING_AGENT_BIN"] = str(fake)
            proc = subprocess.run(
                [sys.executable, str(REPO_ROOT / "scripts" / "agent_runners" / "pi_runner.py")],
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            payload = json.loads(proc.stdout)
            self.assertEqual(proc.returncode, 0)
            self.assertEqual(payload["status"], "succeeded")
            self.assertNotIn("Bearer abcdef", payload["stdout_tail"])
            self.assertNotIn("sk-abcdefghijklmnopqrstuvwxyz", payload["stderr_tail"])
            self.assertLessEqual(len(payload["stdout_tail"]), 4200)
            self.assertLessEqual(len(payload["stderr_tail"]), 4200)

    def test_droid_adapter_uses_verified_exec_file_cwd_shape(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            fake = Path(tmp) / "fake_droid.py"
            fake.write_text(
                "#!/usr/bin/env python3\n"
                "import json, sys\n"
                "print(json.dumps(sys.argv[1:]))\n",
                encoding="utf-8",
            )
            fake.chmod(0o755)
            env = adapter_env(json.dumps({"cwd": tmp}))
            env["TASK_AGENT"] = "droid"
            env["DROID_BIN"] = str(fake)
            proc = subprocess.run(
                [sys.executable, str(REPO_ROOT / "scripts" / "agent_runners" / "droid_runner.py")],
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            payload = json.loads(proc.stdout)
            self.assertEqual(proc.returncode, 0)
            argv = json.loads(payload["stdout_tail"])
            self.assertEqual(argv[0], "exec")
            self.assertIn("--file", argv)
            self.assertIn("--cwd", argv)
            self.assertEqual(Path(argv[argv.index("--cwd") + 1]).resolve(), Path(tmp).resolve())

    def test_droid_auth_failure_is_blocked(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            fake = Path(tmp) / "fake_droid.py"
            fake.write_text(
                "#!/usr/bin/env python3\n"
                "import sys\n"
                "print('Authentication failed. Please log in using /login or set FACTORY_API_KEY.', file=sys.stderr)\n"
                "raise SystemExit(1)\n",
                encoding="utf-8",
            )
            fake.chmod(0o755)
            env = adapter_env(json.dumps({"cwd": tmp}))
            env["TASK_AGENT"] = "droid"
            env["DROID_BIN"] = str(fake)
            proc = subprocess.run(
                [sys.executable, str(REPO_ROOT / "scripts" / "agent_runners" / "droid_runner.py")],
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            payload = json.loads(proc.stdout)
            self.assertEqual(proc.returncode, 1)
            self.assertEqual(payload["status"], "blocked")
            self.assertIn("operator action required", payload["summary"])


if __name__ == "__main__":
    unittest.main()
