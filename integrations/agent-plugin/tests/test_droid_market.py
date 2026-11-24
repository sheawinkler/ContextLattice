import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from backup_profile import SETTINGS
from manage import Conflict
from verify import ID, MARKET, NAME, droid_guide


class DroidMarketTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name).resolve()
        self.profile, self.market = root / ".factory", root / MARKET
        version = self.profile / "plugins/cache" / MARKET / "contextlattice/v1"
        self.guide = version / "skills" / NAME / "SKILL.md"
        self.guide.parent.mkdir(parents=True)
        self.guide.write_bytes(b"fixture")
        source = {"source": "local", "path": str(self.market)}
        self.settings = {"enabledPlugins": {ID: True}, "extraKnownMarketplaces": {MARKET: {"source": source}}}
        self.known = {MARKET: {"source": source, "installLocation": str(self.market)}}
        self.install = {"version": "v1", "scope": "user", "source": MARKET, "installPath": str(version)}
        self.active = f"Installed plugins:\nActive:\n  {ID}  [user]  v1\n".encode()

    def lookup(self, output=None):
        payloads = (self.settings, self.known, {"plugins": {ID: [self.install]}})
        for key, data in zip(SETTINGS["droid"], payloads, strict=True):
            (self.profile / key).write_text(json.dumps(data))
        return droid_guide(self.profile, self.market, self.active if output is None else output)

    def test_native_cache_and_three_non_auth_records(self):
        self.assertEqual(self.lookup(), self.guide)
        self.assertEqual(set(SETTINGS["droid"]), {"settings.json", "plugins/known_marketplaces.json",
                                                  "plugins/installed_plugins.json"})

    def test_active_user_and_source_are_required(self):
        for output in (b"No plugins installed", self.active.replace(b"Active:", b"Disabled:"),
                       self.active.replace(b"[user]", b"[project]"), self.active + self.active):
            with self.subTest(output=output), self.assertRaises(Conflict):
                self.lookup(output)
        self.known[MARKET]["installLocation"] = str(self.market.parent)
        with self.assertRaises(Conflict):
            self.lookup()

    def test_refuses_path_escape_and_symlink(self):
        self.install["version"] = ".."
        with self.assertRaises(Conflict):
            self.lookup()
        self.install["version"] = "v1"
        adjacent = self.guide.with_suffix(".other")
        self.guide.rename(adjacent)
        self.guide.symlink_to(adjacent)
        with self.assertRaises(Conflict):
            self.lookup()
