#!/usr/bin/env python3
"""Focused tests for the cross-lane public-core capability gate."""

from __future__ import annotations

import importlib.machinery
import importlib.util
import sys
import types
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
AGENT_SCRIPTS = REPO_ROOT / "scripts" / "agent"
if str(AGENT_SCRIPTS) not in sys.path:
    sys.path.insert(0, str(AGENT_SCRIPTS))


def load_audit() -> types.ModuleType:
    path = AGENT_SCRIPTS / "audit-capability-parity"
    loader = importlib.machinery.SourceFileLoader("audit_capability_parity", str(path))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    if spec is None:
        raise RuntimeError("failed to create capability-parity module spec")
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class CapabilityParityTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.audit = load_audit()

    def result(self, ref: str, capabilities: dict[str, dict[str, object]]) -> dict[str, object]:
        return {
            "ref": ref,
            "version": 1,
            "capability_ids": sorted(capabilities),
            "capabilities": capabilities,
        }

    def test_worktree_manifest_and_artifacts_validate(self) -> None:
        result = self.audit.inspect_ref("WORKTREE", self.audit.DEFAULT_MANIFEST)
        self.assertTrue(result["ok"], result.get("findings"))
        self.assertGreaterEqual(result["capability_count"], 10)

    def test_frontier_t1_public_core_is_declared(self) -> None:
        result = self.audit.inspect_ref("WORKTREE", self.audit.DEFAULT_MANIFEST)
        self.assertTrue(result["ok"], result.get("findings"))
        self.assertEqual(result["release_train"], "v3.19")
        self.assertTrue(
            {
                "task_identity_reconciliation.v1",
                "objective_graph.v1",
                "decision_change.v1",
            }.issubset(result["capability_ids"]),
            result["capability_ids"],
        )

    def test_missing_capability_is_detected(self) -> None:
        capability = {"id": "context_pack.v1", "summary": "bounded context", "contracts": [], "artifacts": ["README.md"]}
        findings = self.audit.compare_results(
            [
                self.result("public", {"context_pack.v1": capability}),
                self.result("paid", {}),
            ]
        )
        self.assertEqual(findings[0]["reason"], "public_core_capability_set_drift")
        self.assertEqual(findings[0]["missing_from_ref"], ["context_pack.v1"])

    def test_definition_drift_is_detected(self) -> None:
        public = {"id": "context_pack.v1", "summary": "bounded context", "contracts": [], "artifacts": ["README.md"]}
        private = {**public, "summary": "different semantics"}
        findings = self.audit.compare_results(
            [
                self.result("public", {"context_pack.v1": public}),
                self.result("private", {"context_pack.v1": private}),
            ]
        )
        self.assertEqual(findings[0]["reason"], "public_core_capability_definition_drift")
        self.assertEqual(findings[0]["capability_id"], "context_pack.v1")


if __name__ == "__main__":
    unittest.main()
