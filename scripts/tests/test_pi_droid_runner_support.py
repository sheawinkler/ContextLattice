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
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPTS_DIR = REPO_ROOT / "scripts"
sys.path.insert(0, str(SCRIPTS_DIR))

from agent_contracts import attach_format_contract, load_agent_contracts_registry, validate_agent_contract_payload  # noqa: E402


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
    def test_profiles_and_contracts_present(self) -> None:
        profiles = json.loads((REPO_ROOT / "config" / "agents" / "agent_profiles.json").read_text())
        self.assertEqual(profiles["profiles"]["pi"]["agent_id"], "pi_agent")
        self.assertEqual(profiles["profiles"]["droid"]["agent_id"], "droid_agent")
        self.assertEqual(profiles["profiles"]["pi"]["state_authority"], "manual")
        self.assertEqual(profiles["profiles"]["droid"]["state_authority"], "manual")
        self.assertIn("agent_state", profiles["adapter_contract"]["required_phases"])
        self.assertIn("pi-coding-agent", profiles["profiles"]["pi"]["surfaces"])
        self.assertIn("brew-cask", profiles["profiles"]["droid"]["surfaces"])

        registry = load_agent_contracts_registry()
        for contract_id in ("runner_capability.v1", "runner_result.v1", "agent_task_lease.v1"):
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
        for contract_id, payload in (
            ("runner_capability.v1", pi_capability),
            ("runner_capability.v1", droid_capability),
            ("runner_result.v1", runner_result),
            ("agent_task_lease.v1", lease),
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

    def test_approval_required_blocks_before_adapter_execution(self) -> None:
        worker = load_task_worker()
        posts: list[tuple[str, dict[str, Any]]] = []
        ran = {"adapter": False}

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
            posts.append((path, payload))
            return {"ok": True}

        def fake_run_adapter(*_: Any, **__: Any) -> dict[str, Any]:
            ran["adapter"] = True
            return {}

        original_runtime, original_post, original_run = worker.ContextExpansionRuntime, worker._post, worker._run_adapter
        try:
            worker.ContextExpansionRuntime = FakeRuntime
            worker._post = fake_post
            worker._run_adapter = fake_run_adapter
            worker._handle_task(
                "http://127.0.0.1:8075",
                {"id": "task1", "title": "Needs approval", "project": "contextlattice", "agent": "pi", "payload": {}, "approval_required": True},
                "pi",
                "auto",
                "model",
                None,
                None,
            )
        finally:
            worker.ContextExpansionRuntime, worker._post, worker._run_adapter = original_runtime, original_post, original_run
        self.assertFalse(ran["adapter"])
        self.assertEqual(posts[-1][1]["status"], "blocked")

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


if __name__ == "__main__":
    unittest.main()
