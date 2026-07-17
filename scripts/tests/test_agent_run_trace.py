from __future__ import annotations

import importlib.util
import sys
import unittest
from importlib.machinery import SourceFileLoader
from pathlib import Path
from types import SimpleNamespace


ROOT = Path(__file__).resolve().parents[2]
AGENT_DIR = ROOT / "scripts/agent"
SCRIPT = AGENT_DIR / "agent-run-trace"


def load_trace_module():
    sys.path.insert(0, str(AGENT_DIR))
    loader = SourceFileLoader("contextlattice_agent_run_trace_test", str(SCRIPT))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    if spec is None:
        raise RuntimeError("failed to create agent-run-trace module spec")
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


TRACE = load_trace_module()


def proof_payload() -> dict:
    return {
        "ok": True,
        "schema_id": "agent_proof_timeline.v1",
        "session": {"id": "sess/proof", "agent": "codex", "status": "completed"},
        "integrity": {"status": "verified", "complete": True, "source_anchors_stable": True},
        "metrics": {
            "joined_row_count": 1,
            "source_row_count": 1,
            "eligible_exact_link_coverage": 1,
            "redaction_count": 0,
            "display_compacted_count": 2,
        },
        "stage_order": ["context", "action"],
        "stages": {
            "context": {"status": "present", "count": 1},
            "action": {"status": "missing", "count": 0},
        },
        "gaps": [{"code": "stage_missing", "source": "timeline", "detail": "action is absent"}],
        "timeline": [
            {
                "ordered_at": "2026-07-16T05:00:00Z",
                "stage": "context",
                "source": "agent_session",
                "summary": "context linked",
            }
        ],
        "rollback": {
            "env": "CONTEXTLATTICE_AGENT_PROOF_TIMELINE_ENABLED=false",
            "fallback_schema": "agent_run_trace.v1",
        },
    }


class AgentRunTraceTests(unittest.TestCase):
    def test_proof_renderers_surface_integrity_gaps_and_timeline(self) -> None:
        tree = TRACE.render_proof_tree(proof_payload())
        markdown = TRACE.render_proof_markdown(proof_payload())
        self.assertIn("receipt: verified", tree)
        self.assertIn("stage_missing", tree)
        self.assertIn("context linked", tree)
        self.assertIn("compacted 2", tree)
        self.assertIn("Integrity: **verified**", markdown)
        self.assertIn("Display compactions: `2`", markdown)
        self.assertIn("`stage_missing`", markdown)

    def test_fetch_proof_uses_canonical_escaped_route(self) -> None:
        captured: dict = {}
        original = TRACE.request_json

        def fake_request(method, path, payload, timeout):
            captured.update(method=method, path=path, payload=payload, timeout=timeout)
            return proof_payload()

        TRACE.request_json = fake_request
        try:
            result = TRACE.fetch_trace(
                SimpleNamespace(session_id="sess/proof", project="contextlattice", proof=True, timeout=7.5)
            )
        finally:
            TRACE.request_json = original
        self.assertEqual(result["schema_id"], "agent_proof_timeline.v1")
        self.assertEqual(captured["method"], "GET")
        self.assertEqual(captured["path"], "/v1/agents/sessions/sess%2Fproof/proof-timeline")
        self.assertEqual(captured["timeout"], 7.5)


if __name__ == "__main__":
    unittest.main()
