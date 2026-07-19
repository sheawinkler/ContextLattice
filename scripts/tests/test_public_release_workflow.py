from __future__ import annotations

import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/public-release-installers.yml"
PAYLOAD_BUILDER = ROOT / "scripts/build_release_payload.sh"


def workflow_job(workflow: str, name: str) -> str:
    match = re.search(
        rf"(?ms)^  {re.escape(name)}:\n(.*?)(?=^  [a-z0-9-]+:\n|\Z)",
        workflow,
    )
    if match is None:
        raise AssertionError(f"workflow job is missing: {name}")
    return match.group(1)


class PublicReleaseWorkflowTests(unittest.TestCase):
    def test_runtime_guard_allows_disabled_t1_compatibility_status_only(self) -> None:
        builder = PAYLOAD_BUILDER.read_text(encoding="utf-8")
        match = re.search(r"public_runtime_marker='([^']+)'", builder)
        self.assertIsNotNone(match)
        marker_pattern = match.group(1) if match is not None else ""

        # Public reports a disabled T1 compatibility status, not T1 governance.
        self.assertNotIn("frontier_t1_governance_state", marker_pattern)
        for forbidden in (
            "frontier_delta_packet_automation",
            "frontier_shared_proof_timeline",
            "frontier_t4_retrieval_governance_state",
            "frontier_t5_policy_laboratory_governance_state",
            "frontier_t6_agent_fit_governance_state",
            "frontier_t7_portable_continuation_governance_state",
            "frontier_t8_skill_evolution_governance_state",
            "frontier_t9_continuity_zero_governance_state",
            "frontier_t10_aggregate_governance_state",
        ):
            self.assertIn(forbidden, marker_pattern)

    def test_release_workflow_observes_latency_on_shared_runners(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        mode = "CONTEXTLATTICE_RELEASE_PERFORMANCE_MODE: shared_runner"

        self.assertEqual(workflow.count(mode), 1)
        self.assertIn("scripts/agent/audit-release-lane-tree", workflow)

    def test_publisher_downloads_only_the_three_installers(self) -> None:
        publish = workflow_job(WORKFLOW.read_text(encoding="utf-8"), "publish-assets")
        downloads = re.findall(
            r"(?m)^      - name: Download [^\n]+\n"
            r"        uses: actions/download-artifact@v8\n"
            r"        with:\n"
            r"          name: ([^\n]+)\n"
            r"          path: ([^\n]+)$",
            publish,
        )

        self.assertNotIn("pattern: public-release-*", publish)
        self.assertNotIn("merge-multiple:", publish)
        self.assertNotIn("public-release-tree-proof", publish)
        self.assertEqual(publish.count("uses: actions/download-artifact@v8"), 3)
        self.assertEqual(
            downloads,
            [
                ("public-release-dmg", "dist"),
                ("public-release-linux", "dist"),
                ("public-release-msi", "dist"),
            ],
        )


if __name__ == "__main__":
    unittest.main()
