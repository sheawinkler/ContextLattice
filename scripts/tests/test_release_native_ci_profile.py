from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "scripts"))
SPEC = importlib.util.spec_from_file_location("release_native_ci_profile", ROOT / "scripts/release_native_ci_profile.py")
PROFILE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROFILE)


class NativeCIProfileTests(unittest.TestCase):
    def test_refuses_default_non_ci_root_and_wrong_runner(self):
        env = {"GITHUB_ACTIONS": "true", "RUNNER_ENVIRONMENT": "github-hosted", "GITHUB_REPOSITORY": "sheawinkler/ContextLattice",
               "GITHUB_WORKFLOW_REF": "sheawinkler/ContextLattice/.github/workflows/preflight.yml@refs/heads/main",
               "GITHUB_EVENT_NAME": "pull_request", "CONTEXTLATTICE_OWNED_NATIVE_CI": "true"}
        self.assertTrue(PROFILE.trusted_runner(env, "linux", "ubuntu:24.04", 1001))
        for key in env:
            wrong = {**env, key: "wrong"}
            self.assertFalse(PROFILE.trusted_runner(wrong, "linux", "ubuntu:24.04", 1001))
        for system, release, uid in (("darwin", "ubuntu:24.04", 1001), ("linux", "ubuntu:26.04", 1001), ("linux", "ubuntu:24.04", 0)):
            self.assertFalse(PROFILE.trusted_runner(env, system, release, uid))

    def test_requires_one_fixed_fresh_causal_setup_witness(self):
        tools = [{"tool": name, "returncode": 1, "stderr": "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted\n", "started_epoch": at, "completed_epoch": at + .2}
                 for name, at in (("msiinfo", 100),)]
        message = 'apparmor="DENIED" operation="capable" profile="unprivileged_userns" comm="bwrap" capname="net_admin"'
        rows = [{"MESSAGE": message, "__REALTIME_TIMESTAMP": str(int(at * 1_000_000))} for at in (100.1,)]
        observation = {"purpose": "benign_bwrap_setup_only", "tools": tools}
        self.assertTrue(PROFILE.attributed_denial(observation, rows))
        for changed in ([], [{**row, "MESSAGE": message.replace("net_admin", "setpcap")} for row in rows], [{**row, "MESSAGE": message.replace("unprivileged_userns", "other")} for row in rows],
                        [{**row, "__REALTIME_TIMESTAMP": "1"} for row in rows]):
            self.assertFalse(PROFILE.attributed_denial(observation, changed))
        for field, value in (("returncode", 0), ("output_truncated", True), ("stderr", "other denial")):
            mutated = json.loads(json.dumps(tools))
            mutated[0][field] = value
            self.assertFalse(PROFILE.attributed_denial({**observation, "tools": mutated}, rows))
        self.assertFalse(PROFILE.attributed_denial({**observation, "tools": tools * 2}, rows))
        self.assertFalse(PROFILE.attributed_denial({"tools": tools}, rows))
        self.assertFalse(PROFILE.attributed_denial({**observation, "tools": [{**tools[0], "tool": "msiextract"}]}, rows))

    def test_setup_witness_cannot_authorize_different_binary_bytes(self):
        identities = [{"path": "/usr/bin/" + name, "sha256": "a" * 64} for name in ("bwrap", "msiinfo", "msiextract")]
        observed = {"bwrap": {"executable": "/usr/bin/bwrap", "sha256": "a" * 64}, "tools": [{"tool": "msiinfo", "executable": "/usr/bin/msiinfo", "sha256": "a" * 64}]}
        self.assertTrue(PROFILE.same_native_tool_identity(observed, identities))
        for field in ("sha256", "executable"):
            changed = json.loads(json.dumps(observed))
            changed["bwrap"][field] = "different"
            self.assertFalse(PROFILE.same_native_tool_identity(changed, identities))
        changed = json.loads(json.dumps(observed))
        changed["tools"][0]["sha256"] = "b" * 64
        self.assertFalse(PROFILE.same_native_tool_identity(changed, identities))

    def test_package_hash_mismatch_never_extracts_or_runs_parser(self):
        package = mock.Mock()
        package.read_bytes.return_value = b"untrusted fixture"
        with mock.patch.object(PROFILE, "run") as run:
            with self.assertRaisesRegex(ValueError, "distribution_package_hash_mismatch"):
                PROFILE.unpack_reviewed_data(package, "0" * 64, Path("unused"), profiles=True)
        run.assert_not_called()

    def test_waits_for_ingestion_without_repeating_probes_or_accepting_old_denials(self):
        tools = [{"tool": name, "returncode": 1, "stderr": "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted\n", "started_epoch": at, "completed_epoch": at + .2}
                 for name, at in (("msiinfo", 100),)]
        message = 'apparmor="DENIED" operation="capable" profile="unprivileged_userns" comm="bwrap" capname="net_admin"'
        rows = [{"MESSAGE": message, "__REALTIME_TIMESTAMP": str(int(at * 1_000_000))} for at in (100.1,)]
        observation = {"purpose": "benign_bwrap_setup_only", "tools": tools}
        report = {}
        with mock.patch.object(PROFILE.subprocess, "run", side_effect=[subprocess.CompletedProcess([], 1, b"", b""),
                subprocess.CompletedProcess([], 0, b"\n".join(json.dumps(row).encode() for row in rows), b"")]) as run, \
                mock.patch.object(PROFILE, "probe") as probe, mock.patch.object(PROFILE.time, "sleep"):
            self.assertEqual(PROFILE.wait_attributed_denials(observation, report), rows)
        self.assertEqual(report["kernel_read_attempts"], 2)
        self.assertEqual(run.call_count, 2)
        probe.assert_not_called()
        self.assertTrue(all(call.args[0][2] == "/usr/bin/journalctl" for call in run.call_args_list))
        stale = [{**row, "__REALTIME_TIMESTAMP": "1"} for row in rows]
        with mock.patch.object(PROFILE.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, b"\n".join(json.dumps(row).encode() for row in stale), b"")), \
                mock.patch.object(PROFILE.time, "monotonic", side_effect=[0, 3]):
            observed = PROFILE.wait_attributed_denials(observation, {})
        self.assertFalse(PROFILE.attributed_denial(observation, observed))

    def test_only_compiled_distribution_add_no_global_policy_fallback(self):
        source = (ROOT / "scripts/release_native_ci_profile.py").read_text()
        self.assertIn('"--binary", "--add"', source)
        self.assertIn('"--config-file", "/dev/null", "--skip-cache"', source)
        for forbidden in ('"--replace"', '"--Complain"', '"sysctl"', '"systemctl"', '"chmod"', '"chown"', 'shell=True'):
            self.assertNotIn(forbidden, source)
