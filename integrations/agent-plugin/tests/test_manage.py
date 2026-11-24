import hashlib
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


KIT = Path(__file__).resolve().parents[1]
MANAGE = KIT / "manage.py"
SKILL = KIT / "plugins/contextlattice/skills/contextlattice-recall/SKILL.md"


class InstallTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name).resolve()
        self.backups = self.root / "backups"
        self.backups.mkdir()

    def run_tool(self, action, kind="skill", *, ok=True, backup=None):
        argv = [sys.executable, "-B", str(MANAGE), action, "--kind", kind, "--root", str(self.root)]
        if backup is not None:
            argv += ["--backup-dir", str(backup)]
        p = subprocess.run(argv, capture_output=True, check=False, text=True)
        self.assertEqual(p.returncode == 0, ok, p.stdout + p.stderr)
        return json.loads(p.stdout)

    def test_owned_round_trips(self):
        unrelated = self.root / "settings.json"
        unrelated.write_bytes(b'{"keep": true}\n')
        for kind, name in (("skill", "contextlattice-recall"), ("marketplace", "contextlattice-local"),
                           ("mercury", "contextlattice")):
            with self.subTest(kind=kind):
                target = self.root / name
                self.assertEqual(self.run_tool("status", kind)["state"], "absent")
                self.assertEqual(self.run_tool("install", kind)["state"], "installed")
                receipt = target / ".contextlattice-install.json"
                old_stat = receipt.stat()
                self.assertEqual(self.run_tool("install", kind)["state"], "unchanged")
                self.assertEqual(receipt.stat().st_mtime_ns, old_stat.st_mtime_ns)
                self.assertEqual(self.run_tool("status", kind)["state"], "installed")
                self.run_tool("uninstall", kind, ok=False)
                backup = self.run_tool("uninstall", kind, backup=self.backups)["backup"]
                saved = self.backups / backup
                for key, digest in json.loads((saved / receipt.name).read_bytes())["sha256"].items():
                    self.assertEqual(hashlib.sha256((saved / key).read_bytes()).hexdigest(), digest)
                self.assertFalse(target.exists())
                self.assertEqual(self.run_tool("uninstall", kind)["state"], "absent")
                self.assertEqual(unrelated.read_bytes(), b'{"keep": true}\n')

    def test_refuses_unowned_even_when_identical(self):
        target = self.root / "contextlattice-recall"
        target.mkdir()
        (target / "SKILL.md").write_bytes(SKILL.read_bytes())
        self.run_tool("install", ok=False)
        self.run_tool("uninstall", ok=False, backup=self.backups)
        self.assertEqual((target / "SKILL.md").read_bytes(), SKILL.read_bytes())

    def test_refuses_local_changes_and_unrelated_children(self):
        self.run_tool("install")
        target = self.root / "contextlattice-recall"
        file = target / "SKILL.md"
        file.write_bytes(b"operator edit")
        self.run_tool("install", ok=False)
        self.run_tool("uninstall", ok=False, backup=self.backups)
        self.assertEqual(file.read_bytes(), b"operator edit")
        file.write_bytes(SKILL.read_bytes())
        extra = target / "other"
        extra.mkdir()
        self.run_tool("uninstall", ok=False, backup=self.backups)
        self.assertTrue(extra.is_dir())
        self.assertFalse(list(self.backups.iterdir()))

    def test_refuses_symlinks_lock_and_nested_backup(self):
        target = self.root / "contextlattice-recall"
        target.symlink_to(self.backups)
        self.run_tool("install", ok=False)
        self.assertFalse(list(self.backups.iterdir()))
        target.unlink()
        lock = self.root / ".contextlattice-recall.install-lock"
        lock.mkdir()
        self.run_tool("install", ok=False)
        self.assertFalse(target.exists())
        lock.rmdir()
        self.run_tool("install")
        self.run_tool("uninstall", ok=False, backup=target)
        file = target / "SKILL.md"
        file.unlink()
        file.symlink_to(SKILL)
        self.run_tool("uninstall", ok=False, backup=self.backups)
        self.assertTrue(file.is_symlink())

    def test_same_payload_in_all_formats(self):
        self.run_tool("install", "marketplace")
        self.run_tool("install", "mercury")
        self.run_tool("install")
        guide = SKILL.read_text()
        source = self.root / "contextlattice-local/plugins/contextlattice"
        self.assertEqual((source / "skills/contextlattice-recall/SKILL.md").read_text(), guide)
        self.assertEqual((self.root / "contextlattice-recall/SKILL.md").read_text(), guide)
        js = (self.root / "contextlattice/index.mjs").read_text().splitlines()[0]
        self.assertEqual(json.loads(js.removeprefix("const guide = ").removesuffix(";")), guide)


if __name__ == "__main__":
    unittest.main()
