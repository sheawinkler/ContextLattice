from __future__ import annotations

import importlib.util
import io
import json
from importlib.machinery import SourceFileLoader
import sys
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
AGENT_DIR = ROOT / "scripts" / "agent"


def load_pack_module():
    sys.path.insert(0, str(AGENT_DIR))
    name = "contextlattice_pack_budget_test_module"
    loader = SourceFileLoader(name, str(AGENT_DIR / "contextlattice-pack"))
    spec = importlib.util.spec_from_loader(name, loader)
    if spec is None or spec.loader is None:
        raise RuntimeError("unable to load contextlattice-pack")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ContextLatticePackBudgetTests(unittest.TestCase):
    def test_tiny_budget_preserves_session_and_agent_identity(self) -> None:
        module = load_pack_module()
        payload = {
            "ok": True,
            "session_id": "sess_budget_identity",
            "agent_id": "agent_budget_identity",
            "task_summary": "identity must survive compaction",
            "context_pack": {
                "query": "bounded identity regression",
                "retrieval_mode": "balanced",
                "retrieval_intent": "decision",
                "ranked_evidence": [{"summary": "x" * 20000}],
            },
        }

        compacted = module.finalize_context_pack_payload(payload, 1024)

        self.assertEqual(compacted.get("session_id"), "sess_budget_identity")
        self.assertEqual(compacted.get("agent_id"), "agent_budget_identity")

    def test_response_mode_delegates_before_pack_request(self) -> None:
        module = load_pack_module()
        delegated: list[list[str]] = []

        class Delegated(Exception):
            pass

        def fake_delegate(argv: list[str]) -> None:
            delegated.append(argv)
            raise Delegated

        argv = ["contextlattice-pack", "response task", "--project", "alpha", "--response", "--retries", "0"]
        with patch.object(module, "delegate_recall_response", side_effect=fake_delegate), patch.object(sys, "argv", argv):
            with self.assertRaises(Delegated):
                module.main()

        self.assertEqual(delegated, [["response task", "--project", "alpha", "--response", "--retries", "0"]])

    def test_default_mode_still_posts_context_pack(self) -> None:
        module = load_pack_module()
        calls: list[str] = []
        timeouts: list[float] = []

        def fake_request(method, path, payload, timeout):
            calls.append(path)
            timeouts.append(timeout)
            return {"ok": True, "context_pack": {"facts": [], "results": []}}

        stdout = io.StringIO()
        argv = ["contextlattice-pack", "default task", "--no-auto-session", "--retries", "0"]
        with patch.dict(module.os.environ, {"CONTEXTLATTICE_CLIENT_TIMEOUT_SECS": ""}), patch.object(module, "request_json", side_effect=fake_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)
        output = json.loads(stdout.getvalue())
        self.assertEqual(calls, ["/memory/context-pack"])
        self.assertEqual(timeouts, [200.0])
        self.assertIn("context_pack", output)
        self.assertEqual(output["format_contract"]["schema_id"], "context_pack_response.v1")

    def test_legacy_pack_timeout_uses_finite_env_or_explicit_override(self) -> None:
        module = load_pack_module()
        cases = (
            ("49", [], 49.0),
            ("49", ["--timeout", "7"], 7.0),
            ("not-a-number", [], 200.0),
        )
        for env_timeout, timeout_args, expected in cases:
            with self.subTest(env_timeout=env_timeout, timeout_args=timeout_args):
                calls: list[float] = []

                def fake_request(method, path, payload, timeout):
                    calls.append(timeout)
                    return {"ok": True, "context_pack": {"facts": [], "results": []}}

                stdout = io.StringIO()
                argv = ["contextlattice-pack", "timeout resolution", "--no-auto-session", *timeout_args]
                with patch.dict(module.os.environ, {"CONTEXTLATTICE_CLIENT_TIMEOUT_SECS": env_timeout}), patch.object(module, "request_json", side_effect=fake_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
                    self.assertEqual(module.main(), 0)
                self.assertEqual(calls, [expected])

    def test_legacy_pack_retry_flag_cannot_replay_post(self) -> None:
        module = load_pack_module()
        calls: list[str] = []

        def failed_request(method, path, payload, timeout):
            calls.append(path)
            raise RuntimeError("simulated one-shot failure")

        stdout = io.StringIO()
        argv = ["contextlattice-pack", "legacy retry budget", "--no-auto-session", "--soft", "--retries", "3", "--budget-chars", "100000"]
        with patch.dict(module.os.environ, {"CONTEXTLATTICE_TRIGGER_SELF_HEAL": "0"}), patch.object(module, "request_json", side_effect=failed_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)

        output = json.loads(stdout.getvalue())
        self.assertEqual(calls, ["/memory/context-pack"])
        self.assertEqual(output["attempts"], 1)
        self.assertEqual(output["status"], "failed_without_retry")
        self.assertEqual(output["retry_policy"], "one_shot_no_replay")


if __name__ == "__main__":
    unittest.main()
