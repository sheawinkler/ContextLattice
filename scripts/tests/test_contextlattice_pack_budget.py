from __future__ import annotations

import importlib.util
from importlib.machinery import SourceFileLoader
import sys
import unittest
from pathlib import Path


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


if __name__ == "__main__":
    unittest.main()
