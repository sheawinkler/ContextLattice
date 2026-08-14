from __future__ import annotations

import copy
import json
import unittest
from pathlib import Path

from scripts.agent_contracts import validate_agent_task_publication_reconciliation


REPO_ROOT = Path(__file__).resolve().parents[2]
FIXTURE_ROOT = REPO_ROOT / "config" / "agent_contracts" / "fixtures"
FIXTURE_GLOB = "agent_task_publication_reconciliation.v1*.json"


class TaskAgentPublicationReconciliationContractTests(unittest.TestCase):
    def fixtures(self) -> list[dict[str, object]]:
        paths = sorted(FIXTURE_ROOT.glob(FIXTURE_GLOB))
        self.assertEqual(len(paths), 4)
        return [json.loads(path.read_text(encoding="utf-8")) for path in paths]

    def test_python_consumer_accepts_exact_core_state_matrix(self) -> None:
        expected = {
            ("writeback_pending", "pending"),
            ("writeback_failed", "failed"),
            ("committed", "committed"),
            ("dead_letter", "dead_letter"),
        }
        observed: set[tuple[str, str]] = set()
        for fixture in self.fixtures():
            findings = validate_agent_task_publication_reconciliation(fixture)
            self.assertEqual(findings, [], msg=f"fixture rejected: {findings}")
            receipt = fixture["publication_receipt"]
            self.assertIsInstance(receipt, dict)
            self.assertEqual(receipt["state"], "staged")
            observed.add((str(fixture["status"]), str(fixture["writeback_status"])))
        self.assertEqual(observed, expected)

    def test_python_consumer_accepts_canonical_explicit_idempotency_key(self) -> None:
        fixture = copy.deepcopy(self.fixtures()[0])
        fixture["idempotency_key"] = "publication-fixture-idempotency"
        self.assertEqual(validate_agent_task_publication_reconciliation(fixture), [])

    def test_python_consumer_rejects_reduced_or_mutated_seams(self) -> None:
        fixture = self.fixtures()[0]
        mutations = []

        reduced = copy.deepcopy(fixture)
        del reduced["cleanup_authorization"]
        mutations.append(reduced)

        extra = copy.deepcopy(fixture)
        extra["result"] = {"unrequested": True}
        mutations.append(extra)

        empty_idempotency = copy.deepcopy(fixture)
        empty_idempotency["idempotency_key"] = "   "
        mutations.append(empty_idempotency)

        oversized_idempotency = copy.deepcopy(fixture)
        oversized_idempotency["idempotency_key"] = "p" * 2049
        mutations.append(oversized_idempotency)

        mutable_receipt = copy.deepcopy(fixture)
        mutable_receipt["status"] = "committed"
        mutable_receipt["writeback_status"] = "committed"
        mutable_receipt["publication_receipt"]["state"] = "committed"
        mutations.append(mutable_receipt)

        foreign_generation = copy.deepcopy(fixture)
        foreign_generation["lease_generation"] = int(foreign_generation["generation"]) + 1
        mutations.append(foreign_generation)

        for mutation in mutations:
            with self.subTest(mutation=mutation):
                self.assertTrue(validate_agent_task_publication_reconciliation(mutation))


if __name__ == "__main__":
    unittest.main()
