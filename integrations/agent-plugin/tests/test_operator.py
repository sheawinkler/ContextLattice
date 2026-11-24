import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


KIT = Path(__file__).resolve().parents[1]


class OperatorTests(unittest.TestCase):
    def test_command_only_launcher_is_scope_bound_and_reversible(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            backups = root / "backups"
            backups.mkdir()
            cmd = [sys.executable, "-B", str(KIT / "manage.py")]
            args = ["--kind", "project-mcp", "--root", str(root), "--backup-dir", str(backups)]
            def call(action, project=None):
                return subprocess.run(cmd + [action, *args] + (["--project", project] if project else []),
                                      capture_output=True)
            self.assertNotEqual(call("install").returncode, 0)
            self.assertEqual(call("install", "scope-one").returncode, 0)
            launcher = root / "contextlattice-project-mcp/launch"
            self.assertEqual(launcher.stat().st_mode & 0o777, 0o700)
            self.assertEqual(json.loads(call("install", "scope-one").stdout)["state"], "unchanged")
            self.assertNotEqual(call("install", "scope-two").returncode, 0)
            self.assertNotEqual(call("uninstall", "scope-two").returncode, 0)
            self.assertTrue(launcher.exists())
            result = json.loads(call("uninstall", "scope-one").stdout)
            self.assertFalse(launcher.exists())
            self.assertTrue((backups / result["backup"] / "launch").is_file())

    def test_backup_only_selected_settings(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            profile, backups = root / "profile", root / "backups"
            profile.mkdir()
            backups.mkdir()
            (profile / "settings.json").write_bytes(b'{"unrelated":true}\n')
            (profile / "auth.json").write_bytes(b"do not copy")
            argv = [sys.executable, "-B", str(KIT / "backup_profile.py"), "claude-code",
                    "--profile", str(profile), "--backup-dir", str(backups)]
            result = json.loads(subprocess.check_output(argv))
            dest = backups / result["backup"]
            self.assertEqual((dest / "settings.json").read_bytes(), (profile / "settings.json").read_bytes())
            self.assertFalse((dest / "auth.json").exists())
            self.assertIsNone(json.loads((dest / "snapshot.json").read_bytes())["plugins/installed_plugins.json"])
            self.assertEqual(dest.stat().st_mode & 0o077, 0)
            (profile / "plugins").symlink_to(dest)
            self.assertNotEqual(subprocess.run(argv, capture_output=True).returncode, 0)
            self.assertEqual(len(list(backups.iterdir())), 1)

    def test_project_is_required_and_not_caller_selectable(self):
        for host in ("claude", "codex"):
            cmd = [sys.executable, "-B", str(KIT / "project_mcp.py"), host]
            for args in ([], ["--project", ""], ["--project", 'a"b'], ["--project", "a/b"]):
                self.assertNotEqual(subprocess.run(cmd + args, capture_output=True).returncode, 0)
            first = subprocess.check_output(cmd + ["--project", "scope-one"], text=True)
            second = subprocess.check_output(cmd + ["--project", "scope-two"], text=True)
            self.assertEqual(first.replace("scope-one", "scope-two"), second)
            self.assertNotIn("http", first)
            self.assertNotIn(str(Path.home()), first)
            if host == "claude":
                server = json.loads(first)["mcpServers"]["contextlattice"]
                self.assertEqual(server, {"type": "stdio", "command": "contextlattice",
                                          "args": ["mcp", "--project", "scope-one"]})


if __name__ == "__main__":
    unittest.main()
