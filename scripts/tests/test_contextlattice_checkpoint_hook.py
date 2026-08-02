#!/usr/bin/env python3
import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

HOOK = Path(__file__).resolve().parents[1] / "agent_hooks" / "contextlattice_checkpoint.sh"

class CheckpointHookTests(unittest.TestCase):
    def run_hook(self, readback_text):
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            shutil.copy2(HOOK, root / HOOK.name)
            common = root / "common.sh"
            common.write_text("""#!/usr/bin/env bash
fail(){ echo "$*" >&2; exit 2; }
contextlattice_env(){ :; }
curl_json(){
  if [[ "$2" == */memory/write ]]; then
    printf '%s' '{"ok":true,"source":"test","content_ref":"ref"}'
  else
    printf '%s' "$READBACK_TEXT"
  fi
}
""")
            env = os.environ.copy()
            env["READBACK_TEXT"] = readback_text
            return subprocess.run(
                [str(root / HOOK.name), "--project", "p", "--topic-path", "t", "--file", "notes/target.md", "--content", "payload", "--query", "unique"],
                text=True, capture_output=True, env=env, check=False,
            )

    def test_exact_file_content_passes(self):
        run = self.run_hook("payload")
        self.assertEqual(run.returncode, 0, run.stderr)
        payload = json.loads(run.stdout)
        self.assertTrue(payload["ok"])
        self.assertEqual(payload["readback"]["first_file"], "notes/target.md")
        self.assertEqual(payload["readback"]["count"], 1)

    def test_fails_when_exact_file_content_differs(self):
        run = self.run_hook("different")
        self.assertNotEqual(run.returncode, 0)
        self.assertFalse(json.loads(run.stdout)["ok"])

if __name__ == "__main__":
    unittest.main()
