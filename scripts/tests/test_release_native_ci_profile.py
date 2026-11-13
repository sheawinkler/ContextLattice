from __future__ import annotations

import importlib.util
import json
from pathlib import Path
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
               "GITHUB_EVENT_NAME": "pull_request", "CONTEXTLATTICE_OWNED_NATIVE_CI": "true"}
        self.assertTrue(PROFILE.trusted_runner(env, "linux", "ubuntu:24.04", 1001))
        for key in env:
            wrong = {**env, key: "wrong"}
            self.assertFalse(PROFILE.trusted_runner(wrong, "linux", "ubuntu:24.04", 1001))
        for system, release, uid in (("darwin", "ubuntu:24.04", 1001), ("linux", "ubuntu:26.04", 1001), ("linux", "ubuntu:24.04", 0)):
            self.assertFalse(PROFILE.trusted_runner(env, system, release, uid))

    def test_requires_fresh_exact_attributed_denial_for_both_tools(self):
        tools = [{"tool": name, "returncode": 1, "stderr": "bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted\n", "started_epoch": at, "completed_epoch": at + .2}
                 for name, at in (("msiinfo", 100), ("msiextract", 102))]
        message = 'apparmor="DENIED" operation="capable" profile="unprivileged_userns" comm="bwrap" capname="net_admin"'
        rows = [{"MESSAGE": message, "__REALTIME_TIMESTAMP": str(int(at * 1_000_000))} for at in (100.1, 102.1)]
        self.assertTrue(PROFILE.attributed_denial({"tools": tools}, rows))
        for changed in ([], rows[:1], [{**row, "MESSAGE": message.replace("unprivileged_userns", "other")} for row in rows],
                        [{**row, "__REALTIME_TIMESTAMP": "1"} for row in rows]):
            self.assertFalse(PROFILE.attributed_denial({"tools": tools}, changed))
        for field, value in (("returncode", 0), ("output_truncated", True), ("stderr", "other denial")):
            mutated = json.loads(json.dumps(tools))
            mutated[0][field] = value
            self.assertFalse(PROFILE.attributed_denial({"tools": mutated}, rows))

    def test_package_hash_mismatch_never_extracts_or_runs_parser(self):
        package = mock.Mock()
        package.read_bytes.return_value = b"untrusted fixture"
        with mock.patch.object(PROFILE, "run") as run:
            with self.assertRaisesRegex(ValueError, "distribution_package_hash_mismatch"):
                PROFILE.unpack_reviewed_data(package, "0" * 64, Path("unused"), profiles=True)
        run.assert_not_called()

    def test_only_compiled_distribution_add_no_global_policy_fallback(self):
        source = (ROOT / "scripts/release_native_ci_profile.py").read_text()
        self.assertIn('"--binary", "--add"', source)
        self.assertIn('"--config-file", "/dev/null", "--skip-cache"', source)
        for forbidden in ('"--replace"', '"--Complain"', '"sysctl"', '"systemctl"', '"chmod"', '"chown"', 'shell=True'):
            self.assertNotIn(forbidden, source)
