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

    def test_response_mode_posts_recall_route_and_preserves_session_scope(self) -> None:
        module = load_pack_module()
        calls: list[tuple[str, dict]] = []
        session_calls: list[dict] = []

        def fake_session(**kwargs):
            session_calls.append(kwargs)
            return {"ok": True, "session_id": "sess_response"}

        def fake_request(method, path, payload, timeout):
            calls.append((path, payload))
            return module.failure_recall_response()

        argv = ["contextlattice-pack", "response task", "--project", "alpha", "--response", "--retries", "0"]
        stdout = io.StringIO()
        with patch.object(module, "ensure_agent_session", side_effect=fake_session), patch.object(module, "request_json", side_effect=fake_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)

        self.assertEqual([path for path, _ in calls], ["/memory/recall/response"])
        self.assertEqual(calls[0][1]["session_id"], "sess_response")
        self.assertEqual(session_calls[0]["tags"], ["auto-session", "recall-response"])
        self.assertTrue(session_calls[0]["metadata"]["response_mode"])
        output = json.loads(stdout.getvalue())
        self.assertEqual(output["schema_id"], "recall_response.v1")
        self.assertNotIn("context_pack", output)
        self.assertNotIn("agent_packet", output)
        self.assertTrue(output["format_contract"]["contract_valid"])

    def test_response_mode_preserves_bounded_abstention(self) -> None:
        module = load_pack_module()

        def fake_request(method, path, payload, timeout):
            self.assertEqual(path, "/memory/recall/response")
            return module.failure_recall_response()

        stdout = io.StringIO()
        argv = ["contextlattice-pack", "missing proof", "--response", "--no-auto-session", "--retries", "0"]
        with patch.object(module, "request_json", side_effect=fake_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)
        output = json.loads(stdout.getvalue())
        self.assertEqual(output["schema_id"], "recall_response.v1")
        self.assertEqual(output["state"]["status"], "abstain")
        self.assertEqual(output["classification"]["posture"], "abstain")
        self.assertNotIn("context_pack", output)

    def test_response_mode_rejects_leaking_gateway_shape(self) -> None:
        module = load_pack_module()
        server_response_id = "rr_aaaaaaaaaaaaaaaaaaaaaaaa"

        def fake_request(method, path, payload, timeout):
            response = module.failure_recall_response()
            response["response_id"] = server_response_id
            response["context_pack"] = {"raw": "must not cross the response boundary"}
            return response

        stdout = io.StringIO()
        argv = ["contextlattice-pack", "malformed response", "--response", "--soft", "--no-auto-session", "--retries", "0"]
        with patch.object(module, "request_json", side_effect=fake_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)
        output = json.loads(stdout.getvalue())
        self.assertNotEqual(output["response_id"], server_response_id)
        self.assertEqual(output["classification"]["posture"], "abstain")
        self.assertNotIn("context_pack", output)
        self.assertNotIn("must not cross the response boundary", stdout.getvalue())

    def test_response_mode_soft_failure_is_bounded_abstention(self) -> None:
        module = load_pack_module()

        def unavailable(method, path, payload, timeout):
            raise OSError("gateway unavailable")

        stdout = io.StringIO()
        argv = ["contextlattice-pack", "unavailable task", "--response", "--soft", "--no-auto-session", "--retries", "0"]
        with patch.object(module, "request_json", side_effect=unavailable), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)
        output = json.loads(stdout.getvalue())
        self.assertEqual(output["schema_id"], "recall_response.v1")
        self.assertEqual(output["classification"]["posture"], "abstain")
        self.assertNotIn("context_pack", output)
        self.assertNotIn("structured_failure", output)
        self.assertNotIn("gateway unavailable", stdout.getvalue())

    def test_default_mode_still_posts_context_pack(self) -> None:
        module = load_pack_module()
        calls: list[str] = []

        def fake_request(method, path, payload, timeout):
            calls.append(path)
            return {"ok": True, "context_pack": {"facts": [], "results": []}}

        stdout = io.StringIO()
        argv = ["contextlattice-pack", "default task", "--no-auto-session", "--retries", "0"]
        with patch.object(module, "request_json", side_effect=fake_request), patch.object(sys, "argv", argv), redirect_stdout(stdout):
            self.assertEqual(module.main(), 0)
        output = json.loads(stdout.getvalue())
        self.assertEqual(calls, ["/memory/context-pack"])
        self.assertIn("context_pack", output)
        self.assertEqual(output["format_contract"]["schema_id"], "context_pack_response.v1")


if __name__ == "__main__":
    unittest.main()
