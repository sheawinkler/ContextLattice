from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("release_native_ci_gate", ROOT / "scripts/release_native_ci_gate.py")
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)


class NativeCIGateTests(unittest.TestCase):
    def test_tooling_is_exact_workflow_object_and_tagged_containment_must_match(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary) / "source"
            tool = source / GATE.TOOL_PATHS[-1]
            tool.parent.mkdir(parents=True)
            tool.write_bytes(b"reviewed containment fixture")
            extracted = Path(temporary) / "tooling"
            def show(argv, **kwargs):
                self.assertEqual(argv[:2], ["git", "show"])
                self.assertTrue(argv[2].startswith("a" * 40 + ":scripts/"))
                return subprocess.CompletedProcess(argv, 0, b"reviewed containment fixture", b"")
            with mock.patch.object(GATE.subprocess, "run", side_effect=show) as run:
                hashes = GATE.extract_tooling("a" * 40, extracted, source)
                self.assertEqual(len(hashes), 4)
                self.assertEqual(run.call_count, 4)
                tool.write_bytes(b"changed tagged containment")
                with self.assertRaisesRegex(ValueError, "tagged_audit_containment_differs"):
                    GATE.extract_tooling("a" * 40, extracted, source)
            with mock.patch.object(GATE.subprocess, "run") as run:
                with self.assertRaisesRegex(ValueError, "invalid_workflow_tooling_commit"):
                    GATE.extract_tooling("main", extracted, source)
                run.assert_not_called()

    def test_nonzero_missing_or_artifact_claim_never_passes_toolchain_gate(self):
        with tempfile.TemporaryDirectory() as directory:
            for code, body in ((1, {"ok": True, "artifact_audit": False}), (0, {"ok": False, "artifact_audit": False}), (0, {"ok": True, "artifact_audit": True}), (0, {"ok": True})):
                result = subprocess.CompletedProcess([], code, json.dumps(body).encode(), b"")
                with mock.patch.object(GATE.subprocess, "run", return_value=result):
                    self.assertFalse(GATE.command_report(["fixed"], Path(directory) / "receipt.json"))

    def test_all_native_release_jobs_require_separate_workflow_bound_gate(self):
        for path, jobs in {
            ROOT / ".github/workflows/public-release-installers.yml": ("build-linux-and-msi", "audit-remote-linux-msi"),
            ROOT / ".github/workflows/release-installers.yml": ("build-linux-and-msi", "audit-paid-downloaded-linux-msi", "release-integrity"),
        }.items():
            if not path.exists():
                continue
            source = path.read_text()
            for name in jobs:
                with self.subTest(workflow=path.name, job=name):
                    job = source.split("\n  " + name + ":", 1)[1]
                    import re
                    job = re.split(r"\n  [a-z0-9-]+:\n", job, maxsplit=1)[0]
                    self.assertEqual(job.count("name: Require reviewed native containment before MSI parsing"), 1)
                    self.assertIn("NATIVE_TOOLING_COMMIT: ${{ github.workflow_sha }}", job)
                    self.assertIn('git show "${NATIVE_TOOLING_COMMIT}:scripts/release_native_ci_gate.py"', job)
                    self.assertIn('python3 "$RUNNER_TEMP/release_native_ci_gate.py"', job)
                    audit = "scripts/agent/audit-public-installer-package" if path.name == "public-release-installers.yml" else {
                        "build-linux-and-msi": "      - name: Audit exact final Linux bundle and Windows MSI",
                        "audit-paid-downloaded-linux-msi": "      - name: Run paid integrity and native Linux/MSI audits on downloaded bytes",
                        "release-integrity": "      - name: Match audited digests and rerun required paid audit immediately before promotion",
                    }[name]
                    self.assertLess(job.index("Require reviewed native containment"), job.index(audit))
                    if name == "build-linux-and-msi":
                        self.assertLess(job.index("Require reviewed native containment"), job.index("      - name: Build"))
