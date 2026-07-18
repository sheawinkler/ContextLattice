#!/usr/bin/env python3
import os
import plistlib
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]


class OrbStackSelfHealInstallTests(unittest.TestCase):
    def test_start_installs_shell_init_free_local_launch_payload(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            repo = root / "repo"
            scripts = repo / "scripts"
            scripts.mkdir(parents=True)
            for name in ("orbstack_self_heal.sh", "ensure_docker_runtime.sh"):
                shutil.copy2(REPO_ROOT / "scripts" / name, scripts / name)

            fake_bin = root / "bin"
            fake_bin.mkdir()
            launchctl = fake_bin / "launchctl"
            launchctl.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            launchctl.chmod(0o755)

            home = root / "home"
            global_home = home / ".contextlattice"
            plist_path = home / "Library" / "LaunchAgents" / "self-heal.plist"
            env = os.environ.copy()
            env.update(
                {
                    "HOME": str(home),
                    "PATH": f"{fake_bin}:{env['PATH']}",
                    "CONTEXTLATTICE_GLOBAL_HOME": str(global_home),
                    "CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCHD_PLIST": str(plist_path),
                    "CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCHD_LABEL": "test.contextlattice.self-heal",
                }
            )
            result = subprocess.run(
                ["/bin/bash", str(scripts / "orbstack_self_heal.sh"), "start"],
                cwd=repo,
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)

            installed = global_home / "scripts" / "orbstack_self_heal.sh"
            ensure = global_home / "scripts" / "ensure_docker_runtime.sh"
            self.assertTrue(installed.is_file())
            self.assertTrue(ensure.is_file())
            self.assertTrue(os.access(installed, os.X_OK))

            with plist_path.open("rb") as handle:
                payload = plistlib.load(handle)
            args = payload["ProgramArguments"]
            self.assertEqual(
                args,
                ["/bin/bash", str(installed), "run-once", "--event", "launchd"],
            )
            self.assertNotIn("-lc", args)
            self.assertNotIn(str(repo), " ".join(args))
            self.assertEqual(payload["EnvironmentVariables"]["DOCKER_CONTEXT"], "orbstack")
            self.assertTrue(payload["StandardOutPath"].startswith(str(global_home)))
            self.assertEqual(payload["StandardOutPath"], payload["StandardErrorPath"])

    def test_run_once_does_not_restart_vm_by_default(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            calls = root / "orb-calls"
            for name, body in {
                "docker": "#!/bin/sh\nexit 1\n",
                "orb": f"#!/bin/sh\nprintf '%s\\n' \"$*\" >> {calls}\nexit 0\n",
            }.items():
                path = fake_bin / name
                path.write_text(body, encoding="utf-8")
                path.chmod(0o755)

            runtime = root / "runtime"
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{fake_bin}:{env['PATH']}",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_RUNTIME_DIR": str(runtime),
                    "CONTEXTLATTICE_ORBSTACK_HEAL_DOCKER_TIMEOUT_SECS": "0.2",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_HEALTH_TIMEOUT_SECS": "0.2",
                    "CONTEXTLATTICE_HEAL_ORCH_URL": "http://127.0.0.1:1/health",
                }
            )
            result = subprocess.run(
                [
                    "/bin/bash",
                    str(REPO_ROOT / "scripts" / "orbstack_self_heal.sh"),
                    "run-once",
                    "--event",
                    "test",
                ],
                cwd=REPO_ROOT,
                env=env,
                text=True,
                capture_output=True,
                check=False,
                timeout=10,
            )
            self.assertEqual(result.returncode, 1)
            self.assertIn('"action":"docker_unavailable_no_restart"', result.stdout)
            self.assertFalse(calls.exists(), "default unattended recovery called orb")


if __name__ == "__main__":
    unittest.main()
