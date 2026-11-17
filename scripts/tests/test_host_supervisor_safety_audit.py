from __future__ import annotations

import importlib.machinery
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
AUDIT = ROOT / "scripts/agent/audit-host-supervisor-safety"
GUARD_EXPECTED_COMPANIONS = frozenset({
    "scripts/agent/audit-host-supervisor-safety",
    "scripts/tests/test_host_supervisor_safety_audit.py",
    "scripts/tests/test_installer_transaction_readiness.py",
    "AGENTS.md", ".github/pull_request_template.md", "docs/host-supervisor-release-safety.md",
    "scripts/docker_anonymous_public.sh", "scripts/tests/test_docker_anonymous_public.py",
})
FIXTURE_PATHS = (
    Path(".env.example"),
    Path("LICENSE"),
    Path("scripts/orbstack_self_heal.sh"),
    Path("scripts/install_global_agent_tools.sh"),
    Path("scripts/install_retention_runner.sh"),
    Path("scripts/agent/audit-agent-global-install-smoke"),
    Path("scripts/tests/test_orbstack_self_heal_install.py"),
    Path("scripts/tests/test_retention_runner_install.py"),
    Path("scripts/tests/test_host_supervisor_safety_audit.py"),
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
    argv = [sys.executable, str(AUDIT), "--root", str(root), "--syntax-shell",
            os.environ.get("CONTEXTLATTICE_AUDIT_SYNTAX_SHELL", "bash")]
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
    def test_source_guard_mode_accepts_only_finite_critical_paths(self) -> None:
        loader = importlib.machinery.SourceFileLoader("guard_safety_audit_fixture", str(AUDIT))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        audit = importlib.util.module_from_spec(spec)
        with mock.patch.dict(sys.modules, {loader.name: audit}):
            loader.exec_module(audit)
        expected = {
            "scripts/agent/audit-host-supervisor-safety",
            "scripts/tests/test_host_supervisor_safety_audit.py",
            "scripts/tests/test_installer_transaction_readiness.py",
        }
        self.assertEqual(audit.SOURCE_GUARD_CRITICAL_PATHS, expected)
        body = "\n".join("- [x] " + label for label in audit.SOURCE_GUARD_PR_CHECKS)
        self.assertEqual(audit.SOURCE_GUARD_COMPANION_PATHS, GUARD_EXPECTED_COMPANIONS)
        syntax = subprocess.CompletedProcess(["mocked-syntax-check"], 0, "", "")
        with mock.patch.object(audit.shutil, "which", return_value="/mocked/zsh"), mock.patch.object(
            audit.subprocess, "run", return_value=syntax
        ):
            for paths in ([path] for path in sorted(expected)):
                with self.subTest(paths=paths):
                    payload = audit.run_audit(ROOT, paths, body, syntax_shell="zsh")
                    self.assertTrue(payload["ok"], payload["failures"])
                    self.assertEqual(payload["evidence_scope"], "source_merge")
                    self.assertIs(payload["release_qualified"], False)
            for companion in audit.SOURCE_GUARD_COMPANION_PATHS - expected:
                with self.subTest(companion=companion):
                    payload = audit.run_audit(ROOT, [next(iter(expected)), companion], body, syntax_shell="zsh")
                    self.assertNotIn("pr_evidence", failure_ids(payload))
            payload = audit.run_audit(ROOT, sorted(expected) + [
                "scripts/docker_anonymous_public.sh", "scripts/tests/test_docker_anonymous_public.py"
            ], body, syntax_shell="zsh")
            self.assertTrue(payload["ok"], payload["failures"])

    def test_source_guard_mode_rejects_scope_and_checkbox_conflicts(self) -> None:
        loader = importlib.machinery.SourceFileLoader("guard_scope_audit_fixture", str(AUDIT))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        audit = importlib.util.module_from_spec(spec)
        with mock.patch.dict(sys.modules, {loader.name: audit}):
            loader.exec_module(audit)
        body = "\n".join("- [x] " + label for label in audit.SOURCE_GUARD_PR_CHECKS)
        entry = "scripts/tests/test_installer_transaction_readiness.py"
        syntax = subprocess.CompletedProcess(["mocked-syntax-check"], 0, "", "")
        with mock.patch.object(audit.shutil, "which", return_value="/mocked/zsh"), mock.patch.object(
            audit.subprocess, "run", return_value=syntax
        ):
            for extra in ("packaging/linux/ContextLattice-Install.sh", "packaging/windows/Install-ContextLattice.ps1",
                          "scripts/orbstack_self_heal.sh", "scripts/retention_runner.sh",
                          "scripts/install_retention_runner.sh", "scripts/build_windows_msi.sh", ".env.example",
                          "services/gateway-go/runtime.go", "contextlattice-dashboard/app/page.tsx", "docker-compose.yml",
                          "scripts/other_runtime.sh", "scripts/build_release_payload.py",
                          ".env.production", "config/env/strict_runtime.env", ".github/workflows/new-scheduled.yml",
                          "../scripts/orbstack_self_heal.sh", "scripts//orbstack_self_heal.sh"):
                with self.subTest(extra=extra):
                    self.assertIn("pr_evidence", failure_ids(audit.run_audit(ROOT, [entry, extra], body, syntax_shell="zsh")))
            self.assertIn("pr_evidence", failure_ids(audit.run_audit(ROOT, ["docs/ordinary.md"], body, syntax_shell="zsh")))
            for label in audit.SOURCE_GUARD_PR_CHECKS:
                incomplete = body.replace("- [x] " + label, "- [ ] " + label)
                self.assertIn("pr_evidence", failure_ids(audit.run_audit(ROOT, [entry], incomplete, syntax_shell="zsh")))
            unknown = body.replace(audit.SOURCE_GUARD_PR_CHECKS[0], "Unknown source-guard scope.")
            self.assertIn("pr_evidence", failure_ids(audit.run_audit(ROOT, [entry], unknown, syntax_shell="zsh")))
            for conflicting in (audit.NO_IMPACT_CHECK, audit.BOOTSTRAP_PR_CHECKS[0],
                                audit.BOOTSTRAP_PR_CHECKS[1], audit.REQUIRED_PR_CHECKS[0],
                                audit.REQUIRED_PR_CHECKS[2]):
                self.assertIn("pr_evidence", failure_ids(audit.run_audit(
                    ROOT, [entry], body + "\n- [x] " + conflicting, syntax_shell="zsh"
                )))

    def test_source_guard_mode_cannot_waive_existing_failure_controls(self) -> None:
        loader = importlib.machinery.SourceFileLoader("guard_controls_audit_fixture", str(AUDIT))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        audit = importlib.util.module_from_spec(spec)
        with mock.patch.dict(sys.modules, {loader.name: audit}):
            loader.exec_module(audit)
        body = "\n".join("- [x] " + label for label in audit.SOURCE_GUARD_PR_CHECKS)
        syntax = subprocess.CompletedProcess(["mocked-syntax-check"], 0, "", "")
        with tempfile.TemporaryDirectory() as temp_dir, mock.patch.object(
            audit.shutil, "which", return_value="/mocked/zsh"
        ), mock.patch.object(audit.subprocess, "run", return_value=syntax):
            root = Path(temp_dir)
            make_fixture(root)
            entry = "scripts/agent/audit-host-supervisor-safety"
            supervisor_tests = root / "scripts/tests/test_orbstack_self_heal_install.py"
            original = supervisor_tests.read_text(encoding="utf-8")
            supervisor_tests.write_text(original.replace("def test_stop_waits_for_confirmed_unload_before_removing_plist(",
                                                          "def removed_failure_control(", 1), encoding="utf-8")
            self.assertIn("failure_injection_tests", failure_ids(audit.run_audit(root, [entry], body, syntax_shell="zsh")))
            supervisor_tests.write_text(original, encoding="utf-8")
            own_tests = root / "scripts/tests/test_host_supervisor_safety_audit.py"
            own_tests.write_text(own_tests.read_text(encoding="utf-8").replace(
                "def test_source_guard_mode_accepts_only_finite_critical_paths(", "def removed_guard_regression(", 1
            ), encoding="utf-8")
            self.assertIn("source_guard_regression_tests", failure_ids(audit.run_audit(root, [entry], body, syntax_shell="zsh")))

    def test_bootstrap_source_merge_is_finite_and_not_release_authority(self) -> None:
        loader = importlib.machinery.SourceFileLoader("bootstrap_safety_audit_fixture", str(AUDIT))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        audit = importlib.util.module_from_spec(spec)
        with mock.patch.dict(sys.modules, {loader.name: audit}):
            loader.exec_module(audit)
        body = "\n".join("- [x] " + label for label in audit.BOOTSTRAP_PR_CHECKS)
        entry = "packaging/linux/ContextLattice-Install.sh"
        syntax = subprocess.CompletedProcess(["mocked-syntax-check"], 0, "", "")
        with mock.patch.object(audit.shutil, "which", return_value="/mocked/zsh"), mock.patch.object(
            audit.subprocess, "run", return_value=syntax
        ) as run:
            for paths in ([entry], sorted(audit.BOOTSTRAP_SOURCE_PATHS)):
                with self.subTest(paths=paths):
                    case_body = body
                    if set(paths) & audit.CLI_NATIVE_TOOL_COMPANION_PATHS:
                        case_body += "\n- [x] " + audit.CLI_NATIVE_TOOL_PR_CHECK
                    payload = audit.run_audit(ROOT, paths, case_body, syntax_shell="zsh")
                    self.assertTrue(payload["ok"], payload["failures"])
                    self.assertEqual(payload["evidence_scope"], "source_merge")
                    self.assertIs(payload["release_qualified"], False)
            self.assertEqual(run.call_args.args[0][0], "/mocked/zsh")
            self.assertEqual(run.call_args.kwargs["timeout"], 10)
            for paths in ([entry, "scripts/orbstack_self_heal.sh"],
                          [entry, "scripts/install_retention_runner.sh"],
                          [entry, "scripts/install_global_agent_tools.sh"],
                          [entry, "unclassified-change.py"],
                          ["scripts/tests/test_installer_transaction_readiness.py"],
                          ["scripts/agent/audit-host-supervisor-safety"],
                          ["docs/host-supervisor-release-safety.md"]):
                with self.subTest(invalid_paths=paths):
                    self.assertIn("pr_evidence", failure_ids(audit.run_audit(ROOT, paths, body)))
            for label in audit.BOOTSTRAP_PR_CHECKS:
                incomplete = body.replace("- [x] " + label, "- [ ] " + label)
                self.assertIn("pr_evidence", failure_ids(audit.run_audit(ROOT, [entry], incomplete)))
            contradictory = body + "\n- [x] " + audit.NO_IMPACT_CHECK
            self.assertIn("pr_evidence", failure_ids(audit.run_audit(ROOT, [entry], contradictory)))
            before = run.call_count
            with self.assertRaises(ValueError):
                audit.run_audit(ROOT, syntax_shell="arbitrary-command")
            self.assertEqual(run.call_count, before)

    def test_cli_bootstrap_source_merge_is_exact_and_not_release_authority(self) -> None:
        loader = importlib.machinery.SourceFileLoader("cli_bootstrap_safety_audit_fixture", str(AUDIT))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        audit = importlib.util.module_from_spec(spec)
        with mock.patch.dict(sys.modules, {loader.name: audit}):
            loader.exec_module(audit)
        entry = "services/gateway-go/cmd/contextlattice-agent-tools/update.go"
        main = "services/gateway-go/cmd/contextlattice-agent-tools/main.go"
        cli_paths = {
            entry, main,
            "services/gateway-go/cmd/contextlattice-agent-tools/update_test.go",
            "docs/cli-updates.md",
        }
        guard_paths = {"scripts/agent/audit-host-supervisor-safety",
                       "scripts/tests/test_host_supervisor_safety_audit.py"}
        self.assertIn(entry, audit.HOST_LIFECYCLE_CRITICAL_PATHS)
        self.assertIn(entry, audit.BOOTSTRAP_ENTRY_PATHS)
        self.assertTrue(audit.is_host_lifecycle_critical(entry))
        self.assertFalse(audit.is_host_lifecycle_critical(main))
        self.assertEqual(audit.SOURCE_GUARD_COMPANION_PATHS, GUARD_EXPECTED_COMPANIONS)
        self.assertNotIn(entry, audit.SOURCE_GUARD_CRITICAL_PATHS)
        self.assertEqual(len(audit.BOOTSTRAP_PR_CHECKS), 4)
        body = "\n".join("- [x] " + label for label in audit.BOOTSTRAP_PR_CHECKS)
        syntax = subprocess.CompletedProcess(["mocked-syntax-check"], 0, "", "")
        with mock.patch.object(audit.shutil, "which", return_value="/mocked/zsh"), mock.patch.object(
            audit.subprocess, "run", return_value=syntax
        ):
            for paths in (cli_paths, cli_paths | guard_paths):
                with self.subTest(allowed_paths=paths):
                    payload = audit.run_audit(ROOT, sorted(paths), body, syntax_shell="zsh")
                    self.assertTrue(payload["ok"], payload["failures"])
                    self.assertEqual(payload["evidence_scope"], "source_merge")
                    self.assertIs(payload["release_qualified"], False)
            for extra in (
                "services/gateway-go/runtime.go",
                "services/gateway-go/cmd/contextlattice-agent-tools/other.go",
                "Dockerfile.orchestrator", "Dockerfile.gateway-go", "README.md",
                "docs/wiki/releases.md", "scripts/orbstack_self_heal.sh",
                "scripts/retention_runner.sh", "scripts/install_retention_runner.sh",
                "scripts/install_global_agent_tools.sh", "scripts/other_runtime.sh",
                ".github/workflows/new-scheduled.yml",
            ):
                with self.subTest(rejected_extra=extra):
                    self.assertIn("pr_evidence", failure_ids(audit.run_audit(
                        ROOT, sorted(cli_paths | guard_paths | {extra}), body, syntax_shell="zsh"
                    )))
            for label in audit.BOOTSTRAP_PR_CHECKS:
                incomplete = body.replace("- [x] " + label, "- [ ] " + label)
                self.assertIn("pr_evidence", failure_ids(audit.run_audit(
                    ROOT, sorted(cli_paths), incomplete, syntax_shell="zsh"
                )))
            for invalid_body in ("", body + "\n- [x] " + audit.NO_IMPACT_CHECK,
                                 "\n".join("- [x] " + label for label in audit.SOURCE_GUARD_PR_CHECKS)):
                self.assertIn("pr_evidence", failure_ids(audit.run_audit(
                    ROOT, sorted(cli_paths), invalid_body, syntax_shell="zsh"
                )))
            self.assertIn("pr_evidence", failure_ids(audit.run_audit(ROOT, [main], body, syntax_shell="zsh")))

    def test_cli_native_tool_companions_require_update_entry_and_atomic_attestation(self) -> None:
        loader = importlib.machinery.SourceFileLoader("cli_native_tool_scope_audit_fixture", str(AUDIT))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        audit = importlib.util.module_from_spec(spec)
        with mock.patch.dict(sys.modules, {loader.name: audit}):
            loader.exec_module(audit)
        entry = "services/gateway-go/cmd/contextlattice-agent-tools/update.go"
        installer = "scripts/install_global_agent_tools.sh"
        powershell_installer = "scripts/install_global_agent_tools.ps1"
        native_test = "scripts/tests/test_native_tools_binary_install.py"
        all_companions = {installer, powershell_installer, native_test}
        companion_sets = ({installer}, {powershell_installer}, {native_test},
                          {installer, powershell_installer}, {installer, native_test},
                          {powershell_installer, native_test}, all_companions)
        self.assertEqual(audit.CLI_NATIVE_TOOL_COMPANION_PATHS, all_companions)
        self.assertTrue(all_companions.isdisjoint(audit.BOOTSTRAP_ENTRY_PATHS))
        self.assertEqual(audit.SOURCE_GUARD_COMPANION_PATHS, GUARD_EXPECTED_COMPANIONS)
        body = "\n".join("- [x] " + label for label in audit.BOOTSTRAP_PR_CHECKS)
        attestation = "Global native-tool changes are confined to candidate binary compilation and atomic binary/link/wrapper publication; supervisor installation and scheduled payloads are unchanged."
        self.assertEqual(audit.CLI_NATIVE_TOOL_PR_CHECK, attestation)
        checked = body + "\n- [x] " + attestation
        syntax = subprocess.CompletedProcess(["mocked-syntax-check"], 0, "", "")
        with mock.patch.object(audit.shutil, "which", return_value="/mocked/zsh"), mock.patch.object(
            audit.subprocess, "run", return_value=syntax
        ):
            for companions in companion_sets:
                paths = sorted({entry} | companions)
                with self.subTest(allowed_companions=companions):
                    payload = audit.run_audit(ROOT, paths, checked, syntax_shell="zsh")
                    self.assertTrue(payload["ok"], payload["failures"])
                    self.assertEqual(payload["evidence_scope"], "source_merge")
                    self.assertIs(payload["release_qualified"], False)
                    for missing in (body, body + "\n- [ ] " + attestation,
                                    body + "\n- [x] Global native-tool changes are safe."):
                        self.assertIn("pr_evidence", failure_ids(audit.run_audit(
                            ROOT, paths, missing, syntax_shell="zsh"
                        )))
            for companions in companion_sets:
                for entries in (set(), *({path} for path in sorted(audit.BOOTSTRAP_ENTRY_PATHS - {entry}))):
                    paths = sorted(entries | companions)
                    with self.subTest(missing_cli_entry=paths):
                        self.assertIn("pr_evidence", failure_ids(audit.run_audit(
                            ROOT, paths, checked, syntax_shell="zsh"
                        )))
            for extra in ("scripts/orbstack_self_heal.sh", "scripts/install_retention_runner.sh",
                          "scripts/other_runtime.sh", "scripts/tests/test_other_native_tools.py",
                          "services/gateway-go/runtime.go", "Dockerfile.orchestrator", "README.md"):
                with self.subTest(unrelated_change=extra):
                    self.assertIn("pr_evidence", failure_ids(audit.run_audit(
                        ROOT, sorted({entry, extra} | all_companions), checked, syntax_shell="zsh"
                    )))
            guard_body = "\n".join("- [x] " + label for label in audit.SOURCE_GUARD_PR_CHECKS)
            self.assertIn("pr_evidence", failure_ids(audit.run_audit(
                ROOT, sorted({entry} | all_companions), guard_body, syntax_shell="zsh"
            )))

    def test_installer_paths_require_completed_pr_evidence_without_shell(self) -> None:
        loader = importlib.machinery.SourceFileLoader("host_safety_audit_fixture", str(AUDIT))
        spec = importlib.util.spec_from_loader(loader.name, loader)
        audit = importlib.util.module_from_spec(spec)
        with mock.patch.dict(sys.modules, {loader.name: audit}):
            loader.exec_module(audit)

        paths = (
            "packaging/linux/ContextLattice-Install.sh",
            "packaging/windows/Install-ContextLattice.ps1",
            "packaging/windows/ContextLattice-Install.cmd",
            "scripts/tests/test_installer_transaction_readiness.py",
        )
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            make_fixture(root)
            unchecked = (root / ".github/pull_request_template.md").read_text(encoding="utf-8")
            checked = unchecked.replace("- [ ] This PR is host-lifecycle critical", "- [x] This PR is host-lifecycle critical")
            for prefix in ("The source", "The installed", "Any launchd", "Docker-unavailable", "At least", "Destructive recovery"):
                checked = checked.replace(f"- [ ] {prefix}", f"- [x] {prefix}")

            # Isolate the classifier from the auditor's unrelated shell syntax subprocess.
            syntax = subprocess.CompletedProcess(["mocked-syntax-check"], 0, "", "")
            with mock.patch.object(audit.shutil, "which", return_value="/mocked/bash"), mock.patch.object(
                audit.subprocess, "run", return_value=syntax
            ):
                for path in paths:
                    for body in ("", unchecked):
                        with self.subTest(path=path, evidence="missing" if not body else "unchecked"):
                            payload = audit.run_audit(root, changed_files=[path], pr_body=body)
                            self.assertFalse(payload["ok"])
                            self.assertIn("pr_evidence", failure_ids(payload))
                    with self.subTest(path=path, evidence="checked"):
                        payload = audit.run_audit(root, changed_files=[path], pr_body=checked)
                        self.assertTrue(payload["ok"], payload["failures"])
                        self.assertNotIn("pr_evidence", failure_ids(payload))
                    with self.subTest(path=path, evidence="contradictory"):
                        payload = audit.run_audit(
                            root, changed_files=[path], pr_body=checked + "\n- [x] " + audit.NO_IMPACT_CHECK
                        )
                        self.assertIn("pr_evidence", failure_ids(payload))
                payload = audit.run_audit(root, changed_files=["docs/host-supervisor-release-safety.md"], pr_body="")
                self.assertTrue(payload["ok"], payload["failures"])
                self.assertNotIn("pr_evidence", failure_ids(payload))

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

    def test_signal_handlers_cannot_move_after_lock_acquisition(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            make_fixture(root)
            path = root / "scripts/orbstack_self_heal.sh"
            text = path.read_text(encoding="utf-8")
            handlers = "\n".join(
                (
                    "  trap release_lock EXIT",
                    "  trap 'handle_lock_signal 130' INT",
                    "  trap 'handle_lock_signal 143' TERM",
                )
            )
            self.assertIn(handlers, text)
            text = text.replace(handlers, "", 1)
            text = text.replace("  LOCK_ACQUIRING=0", handlers + "\n  LOCK_ACQUIRING=0", 1)
            path.write_text(text, encoding="utf-8")
            result, payload = run_audit(root)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("signal_safe_lock_acquisition", failure_ids(payload))

    def test_lock_claim_and_acquisition_injection_cannot_be_removed(self) -> None:
        mutations = (
            ("scripts/orbstack_self_heal.sh", 'LOCK_CLAIM="$(mktemp "${LOCK_DIR}.acquire.$$.XXXXXX")"', 'LOCK_CLAIM=""', "signal_safe_lock_acquisition"),
            ("scripts/tests/test_orbstack_self_heal_install.py", "def test_signals_during_lock_acquisition_release_owned_state(", "def removed_acquisition_coverage(", "failure_injection_tests"),
        )
        for relative, before, after, check_id in mutations:
            with self.subTest(path=relative), tempfile.TemporaryDirectory() as temp_dir:
                root = Path(temp_dir)
                make_fixture(root)
                path = root / relative
                text = path.read_text(encoding="utf-8")
                self.assertIn(before, text)
                path.write_text(text.replace(before, after, 1), encoding="utf-8")
                result, payload = run_audit(root)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(check_id, failure_ids(payload))

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
