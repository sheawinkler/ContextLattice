from __future__ import annotations

import re
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = ROOT / ".github/workflows/public-release-installers.yml"
PRODUCT_TRUTH_WORKFLOW = ROOT / ".github/workflows/public-product-truth.yml"
AGENT_CONTEXT_WORKFLOW = ROOT / ".github/workflows/agent-context-gate.yml"
PAYLOAD_BUILDER = ROOT / "scripts/build_release_payload.sh"
OUTER_CONTRACT = ROOT / "scripts/release_installer_outer.py"
PAYLOAD_POLICY = ROOT / "scripts/release_payload_policy.py"
INSTALLER_BUILDERS = {
    "macos": ROOT / "scripts/build_macos_dmg.sh",
    "windows": ROOT / "scripts/build_windows_msi.sh",
    "linux": ROOT / "scripts/build_linux_bundle.sh",
}


def workflow_job(workflow: str, name: str) -> str:
    match = re.search(
        rf"(?ms)^  {re.escape(name)}:\n(.*?)(?=^  [a-z0-9-]+:\n|\Z)",
        workflow,
    )
    if match is None:
        raise AssertionError(f"workflow job is missing: {name}")
    return match.group(1)


class PublicReleaseWorkflowTests(unittest.TestCase):
    def test_public_builders_bind_the_shared_outer_installer_contract(self) -> None:
        self.assertTrue(OUTER_CONTRACT.is_file())
        self.assertTrue(PAYLOAD_POLICY.is_file())

        payload_builder = PAYLOAD_BUILDER.read_text(encoding="utf-8")
        self.assertIn('release_installer_outer.py" contract \\', payload_builder)
        self.assertIn("from release_payload_policy import", payload_builder)
        self.assertIn('[[ "${RELEASE_LANE}" == "public" ]]', payload_builder)
        self.assertNotIn('SOURCE_TRACKING_REF="refs/remotes/public-paid/main"', payload_builder)

        for kind, path in INSTALLER_BUILDERS.items():
            with self.subTest(kind=kind):
                builder = path.read_text(encoding="utf-8")
                self.assertIn('release_installer_outer.py" stage \\', builder)
                self.assertIn('--lane "${RELEASE_LANE}"', builder)

        linux_builder = INSTALLER_BUILDERS["linux"].read_text(encoding="utf-8")
        self.assertIn('release_installer_outer.py" validate \\', linux_builder)
        self.assertIn('release_installer_outer.py" build-linux-archive \\', linux_builder)
        self.assertIn('release_installer_outer.py" validate-linux-archive \\', linux_builder)

    def test_public_payload_zip_emits_the_contracted_root_directory(self) -> None:
        payload_builder = PAYLOAD_BUILDER.read_text(encoding="utf-8")

        self.assertIn(
            'root = zipfile.ZipInfo("contextlattice/", date_time=date_time)',
            payload_builder,
        )
        self.assertIn('root.external_attr = (0o755 << 16) | 0x10', payload_builder)
        self.assertIn('zf.writestr(root, b"")', payload_builder)

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
        self.assertNotIn("CONTEXTLATTICE_RELEASE_PERFORMANCE_MODE", workflow)
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

        self.assertNotRegex(publish, r"(?m)^\s+pattern: public-release-\*\s*$")
        self.assertIn("pattern: public-release-*-package-audit*", publish)
        self.assertIn("merge-multiple: true", publish)
        self.assertNotIn("public-release-tree-proof", publish)
        self.assertEqual(publish.count("uses: actions/download-artifact@v8"), 4)
        self.assertEqual(
            downloads,
            [
                ("public-release-dmg", "dist"),
                ("public-release-linux", "dist"),
                ("public-release-msi", "dist"),
            ],
        )

    def test_fresh_build_jobs_bind_the_approved_public_remote(self) -> None:
        workflow = WORKFLOW.read_text(encoding="utf-8")
        for job_name in ("build-macos-dmg", "build-linux-and-msi"):
            with self.subTest(job=job_name):
                job = workflow_job(workflow, job_name)
                self.assertIn("Bind approved public source authority", job)
                self.assertIn('git remote add public "https://github.com/${APPROVED_SOURCE_REPOSITORY}.git"', job)
                self.assertIn('"+${APPROVED_SOURCE_REF}:${APPROVED_SOURCE_TRACKING_REF}"', job)
                self.assertIn('git merge-base --is-ancestor "$EXPECTED_RELEASE_COMMIT" "$source_ref_tip"', job)
                self.assertLess(job.index("Bind approved public source authority"), job.index("Build public"))

    def test_public_alias_never_becomes_latest_release(self) -> None:
        promotion = workflow_job(WORKFLOW.read_text(encoding="utf-8"), "promote-release")
        self.assertIn('latest_flag=(--latest)', promotion)
        self.assertIn('if [[ "$RELEASE_TAG" == *-public ]]; then', promotion)
        self.assertIn('latest_flag=(--latest=false)', promotion)
        self.assertIn('"${latest_flag[@]}"', promotion)

    def test_active_release_workflow_is_truth_gated_and_actionlint_checked(self) -> None:
        product_truth = PRODUCT_TRUTH_WORKFLOW.read_text(encoding="utf-8")
        agent_context = AGENT_CONTEXT_WORKFLOW.read_text(encoding="utf-8")
        for workflow in (product_truth, agent_context):
            self.assertIn('.github/workflows/public-release-installers.yml', workflow)
            self.assertIn('scripts/task_agent_worker.py', workflow)
            self.assertIn('scripts/tests/test_task_agent_worker_claim_identity.py', workflow)
        self.assertIn('.github/workflows/release-installers.yml', product_truth)
        self.assertGreaterEqual(product_truth.count('scripts/task_agent_worker.py'), 2)
        self.assertGreaterEqual(product_truth.count('scripts/tests/test_task_agent_worker_claim_identity.py'), 2)
        for required in (
            "scripts/agent/audit-public-installer-package",
            "scripts/tests/test_public_release_package_scan.py",
            "scripts/tests/test_public_release_workflow.py",
            "scripts.tests.test_public_release_package_scan",
            "scripts.tests.test_public_release_workflow",
            "scripts.tests.test_task_agent_worker_claim_identity",
            "actionlint",
        ):
            self.assertIn(required, product_truth)


if __name__ == "__main__":
    unittest.main()
