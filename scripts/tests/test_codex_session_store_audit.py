import argparse
import errno
import importlib.machinery
import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "agent" / "audit-codex-session-store"


def load_audit_module():
    sys.path.insert(0, str(SCRIPT.parent))
    loader = importlib.machinery.SourceFileLoader("audit_codex_session_store", str(SCRIPT))
    spec = importlib.util.spec_from_loader(loader.name, loader)
    module = importlib.util.module_from_spec(spec)
    loader.exec_module(module)
    return module


class CodexSessionStoreAuditTest(unittest.TestCase):
    def test_only_literal_os_denials_count_as_permission_evidence(self):
        module = load_audit_module()
        self.assertTrue(module.is_literal_permission_denial(PermissionError(errno.EACCES, "Permission denied")))
        self.assertTrue(module.is_literal_permission_denial(OSError(errno.EPERM, "Operation not permitted")))
        self.assertTrue(module.is_literal_permission_denial(OSError(None, "EACCES")))
        self.assertTrue(module.is_literal_permission_denial(OSError(None, "EPERM")))
        self.assertFalse(module.is_literal_permission_denial(FileNotFoundError(errno.ENOENT, "No such file or directory")))
        self.assertFalse(
            module.is_literal_permission_denial(
                FileNotFoundError(errno.ENOENT, "No such file or directory", "/tmp/eacces-missing")
            )
        )
        self.assertFalse(
            module.is_literal_permission_denial(
                FileNotFoundError(errno.ENOENT, "No such file or directory", "/tmp/eperm-missing")
            )
        )
        self.assertFalse(module.is_literal_permission_denial(OSError(errno.EIO, "Input/output error")))
        self.assertFalse(module.is_literal_permission_denial(OSError(None, "missing /tmp/eacces-missing")))

    def test_audit_reports_literal_denial_from_exact_stat_probe(self):
        module = load_audit_module()
        args = argparse.Namespace(
            codex_home="/synthetic/codex-home",
            sessions_path="",
            sample_limit=1,
            no_write_probe=False,
            strict_warnings=False,
            pretty=False,
        )
        with mock.patch.object(Path, "stat", side_effect=PermissionError(errno.EACCES, "Permission denied")):
            payload = module.audit(args)
        self.assertFalse(payload["ok"])
        self.assertEqual(payload["permission_evidence"]["status"], "confirmed")
        self.assertEqual(payload["permission_evidence"]["literal_denial_count"], 1)
        self.assertTrue(payload["permission_evidence"]["repair_recommended"])
        self.assertEqual(payload["findings"][0]["reason"], "codex_home_stat_failed")

    def test_healthy_symlink_warnings_do_not_recommend_permission_repair(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            codex_home = root / "codex-home"
            target = root / "sessions-target"
            transcript_target = root / "transcript.jsonl"
            codex_home.mkdir()
            target.mkdir()
            transcript_target.write_text("{}\n", encoding="utf-8")
            (codex_home / "sessions").symlink_to(target, target_is_directory=True)
            (target / "session.jsonl").symlink_to(transcript_target)

            completed = subprocess.run(
                [
                    str(SCRIPT),
                    "--codex-home",
                    str(codex_home),
                    "--sample-limit",
                    "5",
                ],
                cwd=ROOT,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            payload = json.loads(completed.stdout)
            self.assertEqual(payload["permission_evidence"]["status"], "not_observed")
            self.assertEqual(payload["permission_evidence"]["literal_denial_count"], 0)
            self.assertFalse(payload["permission_evidence"]["repair_recommended"])
            self.assertEqual(payload["access"], {"stat": True, "list": True, "write": True})
            suggestions = " ".join(finding.get("suggested_fix", "") for finding in payload["findings"])
            self.assertNotIn("grant", suggestions.lower())
            self.assertIn("no permission action", suggestions.lower())


if __name__ == "__main__":
    unittest.main()
