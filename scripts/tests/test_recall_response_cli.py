from __future__ import annotations

import json
import os
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PACK = ROOT / "scripts" / "agent" / "contextlattice-pack"
RECALL = ROOT / "scripts" / "agent" / "contextlattice-recall-response"


class RecallResponseCLITest(unittest.TestCase):
    def test_pack_response_delegates_to_native_alias(self) -> None:
        with tempfile.TemporaryDirectory(prefix="contextlattice-recall-cli-") as temp:
            temp_path = Path(temp)
            args_path = temp_path / "args.json"
            fake = temp_path / "contextlattice-agent-tools"
            fake.write_text(
                "#!/usr/bin/env python3\n"
                "import json, os, sys\n"
                "Path = __import__('pathlib').Path\n"
                "Path(os.environ['ARGS_PATH']).write_text(json.dumps(sys.argv[1:]))\n",
                encoding="utf-8",
            )
            fake.chmod(fake.stat().st_mode | stat.S_IXUSR)
            env = os.environ.copy()
            env["CONTEXTLATTICE_AGENT_TOOLS_BIN"] = str(fake)
            env["ARGS_PATH"] = str(args_path)
            subprocess.run(
                [sys.executable, str(PACK), "same task", "--response", "--raw"],
                cwd=ROOT,
                env=env,
                check=True,
                capture_output=True,
                text=True,
            )
            self.assertEqual(json.loads(args_path.read_text(encoding="utf-8")), ["recall-response", "same task", "--raw"])

    def test_direct_script_preserves_stdout_and_stderr_process_boundary(self) -> None:
        with tempfile.TemporaryDirectory(prefix="contextlattice-recall-cli-") as temp:
            temp_path = Path(temp)
            args_path = temp_path / "args.json"
            fake = temp_path / "contextlattice-agent-tools"
            fake.write_text(
                "#!/usr/bin/env python3\n"
                "import os, sys\n"
                "from pathlib import Path\n"
                "Path(os.environ['ARGS_PATH']).write_text(' '.join(sys.argv[1:]))\n"
                "print('{\\\"ok\\\":true}')\n"
                "print('native notice', file=sys.stderr)\n",
                encoding="utf-8",
            )
            fake.chmod(fake.stat().st_mode | stat.S_IXUSR)
            env = os.environ.copy()
            env["CONTEXTLATTICE_AGENT_TOOLS_BIN"] = str(fake)
            env["ARGS_PATH"] = str(args_path)
            result = subprocess.run(
                [sys.executable, str(RECALL), "same task", "--response"],
                cwd=ROOT,
                env=env,
                check=True,
                capture_output=True,
                text=True,
            )
            self.assertEqual(result.stdout.strip(), '{"ok":true}')
            self.assertEqual(result.stderr.strip(), "native notice")
            self.assertEqual(args_path.read_text(encoding="utf-8"), "recall-response same task")


if __name__ == "__main__":
    unittest.main()
