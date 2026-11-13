from __future__ import annotations

import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "release_native_toolchain_preflight", ROOT / "scripts/release_native_toolchain_preflight.py"
)
PROBE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROBE)


class NativeToolchainPreflightTests(unittest.TestCase):
    def test_non_linux_is_not_native_evidence(self) -> None:
        with mock.patch.object(PROBE.sys, "platform", "darwin"):
            report = PROBE.probe(ROOT)
        self.assertFalse(report["ok"])
        self.assertFalse(report["artifact_audit"])
        self.assertEqual(report["error"], "native_probe_requires_linux")

    def test_controlled_probe_preserves_real_containment_and_clears_environment(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            tool = Path(temporary) / "tool"
            tool.write_bytes(b"fake version tool fixture")
            with mock.patch.object(PROBE.sys, "platform", "linux"), \
                    mock.patch.object(PROBE.shutil, "which", return_value=str(tool)), \
                    mock.patch.object(PROBE.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, b"", b"fixture denied")) as run:
                report = PROBE.probe(ROOT)
            self.assertFalse(report["ok"])
            self.assertEqual(run.call_count, 2)
            for call in run.call_args_list:
                argv = call.args[0]
                self.assertIn("--unshare-all", argv)
                self.assertIn("--clearenv", argv)
                self.assertIn("--die-with-parent", argv)
                self.assertEqual(argv[-1], "--version")
                self.assertEqual(call.kwargs["env"], {"PATH": "/usr/bin:/bin"})
                self.assertEqual(call.kwargs["timeout"], 15)
                self.assertNotIn("shell", call.kwargs)
            self.assertEqual(report["tools"][0]["stderr"], "fixture denied")
            self.assertFalse(report["artifact_audit"])
            self.assertEqual(report["bwrap"]["executable"], str(tool.resolve()))
            self.assertEqual(len(report["bwrap"]["sha256"]), 64)
            self.assertLessEqual(report["tools"][0]["started_epoch"], report["tools"][0]["completed_epoch"])

    def test_workflow_cannot_publish_or_disable_security_policy(self) -> None:
        workflow = (ROOT / ".github/workflows/release-native-toolchain-preflight.yml").read_text()
        self.assertIn("contents: read", workflow)
        self.assertIn("runs-on: ubuntu-24.04", workflow)
        self.assertIn("timeout-minutes: 5", workflow)
        self.assertIn("python3 -m unittest scripts.tests.test_release_native_toolchain_preflight", workflow)
        self.assertIn("native-toolchain-kernel-denials.jsonl", workflow)
        self.assertIn("--since", workflow)
        for forbidden in ("contents: write", "gh release", "sysctl", "apparmor_parser", "chmod", "chown"):
            self.assertNotIn(forbidden, workflow)
