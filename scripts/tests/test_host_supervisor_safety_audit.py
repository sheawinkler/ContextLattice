from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
AUDIT = ROOT / "scripts/agent/audit-host-supervisor-safety"
FIXTURE_PATHS = (
    Path(".env.example"),
    Path("LICENSE"),
    Path("scripts/orbstack_self_heal.sh"),
    Path("scripts/install_global_agent_tools.sh"),
    Path("scripts/install_retention_runner.sh"),
    Path("scripts/agent/audit-agent-global-install-smoke"),
    Path("scripts/tests/test_orbstack_self_heal_install.py"),
    Path("scripts/tests/test_retention_runner_install.py"),
    Path(".github/workflows/public-product-truth.yml"),
    Path(".github/workflows/release-installers.yml"),
    Path(".github/pull_request_template.md"),
    Path("AGENTS.md"),
    Path("docs/host-supervisor-release-safety.md"),
)


def make_fixture(root: Path) -> None:
    for relative in FIXTURE_PATHS:
        source = ROOT / relative
        if not source.exists():
            continue
        target = root / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)


def run_audit(
    root: Path,
    *,
    changed_files: list[str] | None = None,
    pr_body: str = "",
) -> tuple[subprocess.CompletedProcess[str], dict[str, object]]:
    argv = ["python3", str(AUDIT), "--root", str(root)]
    env = os.environ.copy()
    input_text = None
    if changed_files is not None:
        argv.extend(["--changed-files-stdin", "--pr-body-env", "TEST_PR_BODY"])
        env["TEST_PR_BODY"] = pr_body
        input_text = "\n".join(changed_files) + "\n"
    result = subprocess.run(
        argv,
        cwd=ROOT,
        capture_output=True,
        check=False,
        env=env,
        input=input_text,
        text=True,
        timeout=20,
    )
    return result, json.loads(result.stdout)


def failure_ids(payload: dict[str, object]) -> set[str]:
    return {str(item["check_id"]) for item in payload.get("failures", []) if isinstance(item, dict)}


class HostSupervisorSafetyAuditTests(unittest.TestCase):
    def test_repository_contract_passes(self) -> None:
        result, payload = run_audit(ROOT)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(payload["ok"])

    def test_unsafe_vm_restart_default_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            make_fixture(root)
            path = root / "scripts/orbstack_self_heal.sh"
            path.write_text(
                path.read_text(encoding="utf-8").replace(
                    'ALLOW_VM_RESTART="${CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART:-0}"',
                    'ALLOW_VM_RESTART="${CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART:-1}"',
                ),
                encoding="utf-8",
            )
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("fail_closed_defaults", failure_ids(payload))

    @unittest.skipUnless(
        (ROOT / ".github/workflows/release-installers.yml").is_file(),
        "paid release workflow is commercial-only",
    )
    def test_paid_release_cannot_drop_failure_injection(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            make_fixture(root)
            path = root / ".github/workflows/release-installers.yml"
            path.write_text(
                path.read_text(encoding="utf-8").replace(
                    "python3 scripts/tests/test_orbstack_self_heal_install.py",
                    "echo supervisor-test-removed",
                ),
                encoding="utf-8",
            )
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("paid_release_gate", failure_ids(payload))

    def test_install_smoke_requires_unique_launchd_identity(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            make_fixture(root)
            path = root / "scripts/agent/audit-agent-global-install-smoke"
            path.write_text(
                path.read_text(encoding="utf-8").replace(
                    'smoke_launchd_label = f"io.contextlattice.orbstack-self-heal.smoke.{os.getpid()}"',
                    'smoke_launchd_label = "io.contextlattice.orbstack-self-heal"',
                ),
                encoding="utf-8",
            )
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("global_smoke_isolation", failure_ids(payload))

    def test_retention_upgrade_cannot_restore_legacy_35_minute_scheduler(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            make_fixture(root)
            path = root / "scripts/install_retention_runner.sh"
            text = path.read_text(encoding="utf-8")
            text = text.replace(
                'INTERVAL_SECONDS="${RETENTION_INTERVAL_SECONDS:-86400}"',
                'INTERVAL_SECONDS="${RETENTION_INTERVAL_SECONDS:-2100}"',
            )
            text = text.replace('rm -f "$LEGACY_PLIST_PATH"', 'echo legacy-retained')
            path.write_text(text, encoding="utf-8")
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("retention_upgrade_policy", failure_ids(payload))

    def test_retention_sample_config_cannot_override_daily_default(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            make_fixture(root)
            path = root / ".env.example"
            path.write_text(
                path.read_text(encoding="utf-8").replace(
                    "RETENTION_INTERVAL_SECONDS=86400",
                    "RETENTION_INTERVAL_SECONDS=2100",
                ),
                encoding="utf-8",
            )
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("retention_config_default", failure_ids(payload))

    def test_retention_installer_cannot_drop_source_identity_or_rollback_test(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            make_fixture(root)
            installer = root / "scripts/install_retention_runner.sh"
            installer.write_text(
                installer.read_text(encoding="utf-8").replace(
                    "CONTEXTLATTICE_RETENTION_RUNNER_SHA256",
                    "REMOVED_RETENTION_RUNNER_SHA256",
                ),
                encoding="utf-8",
            )
            tests = root / "scripts/tests/test_retention_runner_install.py"
            tests.write_text(
                tests.read_text(encoding="utf-8").replace(
                    "test_failed_replacement_restores_prior_loaded_plist",
                    "removed_rollback_case",
                ),
                encoding="utf-8",
            )
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("retention_install_proof", failure_ids(payload))

    def test_pr_evidence_contract_cannot_be_removed(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            make_fixture(root)
            path = root / ".github/pull_request_template.md"
            path.write_text(
                path.read_text(encoding="utf-8").replace("contextlattice-host-lifecycle-safety", "removed-safety-marker"),
                encoding="utf-8",
            )
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("pr_evidence_contract", failure_ids(payload))

    def test_critical_change_requires_completed_pr_evidence(self) -> None:
        result, payload = run_audit(
            ROOT,
            changed_files=["scripts/orbstack_self_heal.sh"],
            pr_body=(ROOT / ".github/pull_request_template.md").read_text(encoding="utf-8"),
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("pr_evidence", failure_ids(payload))

    def test_completed_pr_evidence_allows_critical_change(self) -> None:
        body = (ROOT / ".github/pull_request_template.md").read_text(encoding="utf-8")
        body = body.replace("- [ ] This PR is host-lifecycle critical", "- [x] This PR is host-lifecycle critical")
        for prefix in ("The source", "The installed", "Any launchd", "Docker-unavailable", "At least", "Destructive recovery"):
            body = body.replace(f"- [ ] {prefix}", f"- [x] {prefix}")
        result, payload = run_audit(
            ROOT,
            changed_files=["scripts/orbstack_self_heal.sh"],
            pr_body=body,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("pr_evidence", failure_ids(payload))

    def test_docs_only_change_does_not_require_operational_evidence(self) -> None:
        result, payload = run_audit(
            ROOT,
            changed_files=["docs/host-supervisor-release-safety.md"],
            pr_body="",
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("pr_evidence", failure_ids(payload))

    def test_public_lane_does_not_require_paid_release_workflow(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            make_fixture(root)
            (root / "LICENSE").write_text("Apache License\nVersion 2.0, January 2004\n", encoding="utf-8")
            (root / ".github/workflows/release-installers.yml").unlink(missing_ok=True)
            result, payload = run_audit(root)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(payload["lane"], "public")
            self.assertNotIn("paid_release_gate", failure_ids(payload))


if __name__ == "__main__":
    unittest.main()
