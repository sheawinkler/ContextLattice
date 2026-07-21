#!/usr/bin/env python3
import json
import os
import plistlib
import subprocess
import tempfile
import time
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
SELF_HEAL = REPO_ROOT / "scripts" / "orbstack_self_heal.sh"
INSTALLER = REPO_ROOT / "scripts" / "install_global_agent_tools.sh"


def write_executable(path: Path, body: str) -> None:
    path.write_text(body, encoding="utf-8")
    path.chmod(0o755)


class OrbStackSelfHealInstallTests(unittest.TestCase):
    def runtime_fixture(self, root: Path) -> tuple[Path, Path, Path, dict[str, str]]:
        fake_bin = root / "bin"
        fake_bin.mkdir()
        runtime = root / "runtime"
        calls = root / "calls"
        health_body = root / "health.json"
        health_body.write_text('{"ok": false}\n', encoding="utf-8")

        write_executable(
            fake_bin / "curl",
            '#!/bin/sh\ncat "$FAKE_HEALTH_BODY"\n',
        )
        write_executable(
            fake_bin / "plutil",
            """#!/bin/sh
body=$(cat | tr -d '[:space:]')
case "$body" in
  '{"ok":true}'|'{"ok":true,'*) printf 'true\\n' ;;
  '{"ok":false}'|'{"ok":false,'*) printf 'false\\n' ;;
  *) exit 1 ;;
esac
""",
        )
        write_executable(
            fake_bin / "ps",
            "#!/bin/sh\nprintf '0.0 OrbStack Helper vmgr\\n'\n",
        )
        write_executable(
            fake_bin / "orb",
            '#!/bin/sh\nprintf \'%s\\n\' "$*" >> "$FAKE_CALLS"\nif [ "$1" = status ]; then echo Stopped; fi\nexit 0\n',
        )
        env = os.environ.copy()
        env.update(
            {
                "PATH": f"{fake_bin}:{env['PATH']}",
                "FAKE_CALLS": str(calls),
                "FAKE_HEALTH_BODY": str(health_body),
                "DOCKER_CONTEXT": "test-orbstack",
                "CONTEXTLATTICE_ORBSTACK_HEAL_RUNTIME_DIR": str(runtime),
                "CONTEXTLATTICE_ORBSTACK_HEAL_DOCKER_TIMEOUT_SECS": "0.2",
                "CONTEXTLATTICE_ORBSTACK_HEAL_HEALTH_TIMEOUT_SECS": "0.2",
                "CONTEXTLATTICE_HEAL_ORCH_URL": "http://127.0.0.1:1/health",
            }
        )
        return fake_bin, runtime, calls, env

    def run_once(self, env: dict[str, str]) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["/bin/bash", str(SELF_HEAL), "run-once", "--event", "test"],
            cwd=REPO_ROOT,
            env=env,
            text=True,
            capture_output=True,
            check=False,
            timeout=10,
        )

    def test_start_installs_shell_init_free_fail_closed_launch_payload(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            calls = root / "launchctl-calls"
            write_executable(
                fake_bin / "launchctl",
                f"#!/bin/sh\nprintf '%s\\n' \"$*\" >> {calls}\nexit 0\n",
            )
            write_executable(fake_bin / "uname", "#!/bin/sh\necho Darwin\n")

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
                ["/bin/bash", str(SELF_HEAL), "start"],
                cwd=REPO_ROOT,
                env=env,
                text=True,
                capture_output=True,
                check=False,
                timeout=10,
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
            self.assertEqual(args, ["/bin/bash", str(installed), "run-once", "--event", "launchd"])
            self.assertNotIn("-lc", args)
            self.assertNotIn(str(REPO_ROOT), " ".join(args))
            policy = payload["EnvironmentVariables"]
            self.assertEqual(policy["DOCKER_CONTEXT"], "orbstack")
            self.assertEqual(policy["CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART"], "0")
            self.assertEqual(policy["CONTEXTLATTICE_ORBSTACK_HEAL_SHED_SERVICES"], "")
            self.assertEqual(policy["CONTEXTLATTICE_ORBSTACK_HEAL_FAILURES_BEFORE_RESTART"], "5")
            self.assertEqual(policy["CONTEXTLATTICE_ORBSTACK_HEAL_HEALTH_FAILURES_BEFORE_REPAIR"], "3")
            self.assertTrue(payload["StandardOutPath"].startswith(str(global_home)))
            self.assertEqual(payload["StandardOutPath"], payload["StandardErrorPath"])

            source = SELF_HEAL.read_text(encoding="utf-8")
            self.assertNotIn("python3", source)
            self.assertNotIn("orb stop --force", source)
            self.assertNotIn("pkill", source)

    def test_run_once_does_not_restart_vm_by_default(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin, _, calls, env = self.runtime_fixture(root)
            write_executable(fake_bin / "docker", "#!/bin/sh\nexit 1\n")

            result = self.run_once(env)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(json.loads(result.stdout)["action"], "docker_unavailable_no_restart")
            self.assertFalse(calls.exists(), "default unattended recovery called orb")

    def test_health_failure_and_high_cpu_never_restart_vm(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin, _, calls, env = self.runtime_fixture(root)
            docker_calls = root / "docker-calls"
            write_executable(
                fake_bin / "docker",
                f"""#!/bin/sh
printf '%s\\n' "$*" >> {docker_calls}
case " $* " in
  *" version "*) echo 29.4.0; exit 0 ;;
  *" ps "*) exit 0 ;;
esac
exit 0
""",
            )
            write_executable(fake_bin / "ps", "#!/bin/sh\nprintf '999.0 OrbStack Helper vmgr\\n'\n")
            env["CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART"] = "1"
            env["CONTEXTLATTICE_ORBSTACK_HEAL_STARTUP_GRACE_SECS"] = "0"

            result = self.run_once(env)
            self.assertEqual(result.returncode, 1)
            self.assertEqual(json.loads(result.stdout)["action"], "health_failure_threshold_wait")
            self.assertFalse(calls.exists(), "health or CPU observation called orb")
            self.assertNotIn(" restart ", f" {docker_calls.read_text(encoding='utf-8')} ")

    def test_forward_repair_is_thresholded_and_latched_per_health_outage(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin, _, calls, env = self.runtime_fixture(root)
            docker_calls = root / "docker-calls"
            write_executable(
                fake_bin / "docker",
                f"""#!/bin/sh
printf '%s\\n' "$*" >> {docker_calls}
case " $* " in
  *" version "*) echo 29.4.0 ;;
  *" ps "*) echo contextlattice-gateway ;;
esac
exit 0
""",
            )
            env["CONTEXTLATTICE_ORBSTACK_HEAL_HEALTH_FAILURES_BEFORE_REPAIR"] = "3"
            env["CONTEXTLATTICE_ORBSTACK_HEAL_FORWARD_REPAIR_COOLDOWN_SECS"] = "0"

            first = self.run_once(env)
            second = self.run_once(env)
            third = self.run_once(env)
            self.assertEqual(json.loads(first.stdout)["action"], "health_failure_threshold_wait")
            self.assertEqual(json.loads(second.stdout)["action"], "health_failure_threshold_wait")
            self.assertEqual(json.loads(third.stdout)["action"], "forward_repair")
            self.assertEqual(third.returncode, 1)

            restart_calls = [
                line for line in docker_calls.read_text(encoding="utf-8").splitlines() if " restart " in f" {line} "
            ]
            self.assertEqual(len(restart_calls), 1)
            fourth = self.run_once(env)
            self.assertEqual(json.loads(fourth.stdout)["action"], "forward_repair_suppressed_outage_latch")
            restart_calls = [
                line for line in docker_calls.read_text(encoding="utf-8").splitlines() if " restart " in f" {line} "
            ]
            self.assertEqual(len(restart_calls), 1)
            self.assertFalse(calls.exists(), "forward repair called orb")

    def test_container_shedding_is_disabled_by_default(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin, _, _, env = self.runtime_fixture(root)
            docker_calls = root / "docker-calls"
            write_executable(
                fake_bin / "docker",
                f"#!/bin/sh\nprintf '%s\\n' \"$*\" >> {docker_calls}\necho 29.4.0\nexit 0\n",
            )
            write_executable(fake_bin / "ps", "#!/bin/sh\nprintf '999.0 OrbStack Helper vmgr\\n'\n")
            Path(env["FAKE_HEALTH_BODY"]).write_text('{"ok": true}\n', encoding="utf-8")

            result = self.run_once(env)
            self.assertEqual(result.returncode, 0, result.stderr)
            calls_text = docker_calls.read_text(encoding="utf-8")
            self.assertNotIn(" stats ", f" {calls_text} ")
            self.assertNotIn(" restart ", f" {calls_text} ")

    def test_vm_restart_requires_threshold_and_is_latched_per_outage(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin, _, calls, env = self.runtime_fixture(root)
            docker_calls = root / "docker-calls"
            write_executable(
                fake_bin / "docker",
                f"#!/bin/sh\nprintf '%s\\n' \"$*\" >> {docker_calls}\nexit 1\n",
            )
            env.update(
                {
                    "CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART": "1",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_FAILURES_BEFORE_RESTART": "3",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_STARTUP_GRACE_SECS": "0",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_POST_RESTART_GRACE_SECS": "0",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_COOLDOWN_SECS": "0",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_RESTART_READY_ATTEMPTS": "1",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_RESTART_READY_INTERVAL_SECS": "0.01",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_MAX_RESTARTS_PER_WINDOW": "5",
                }
            )

            first = self.run_once(env)
            second = self.run_once(env)
            self.assertEqual(json.loads(first.stdout)["action"], "docker_failure_threshold_wait")
            self.assertEqual(json.loads(second.stdout)["action"], "docker_failure_threshold_wait")
            self.assertFalse(calls.exists())

            third = self.run_once(env)
            self.assertEqual(third.returncode, 1)
            orb_calls_after_attempt = calls.read_text(encoding="utf-8").splitlines()
            self.assertEqual([line for line in orb_calls_after_attempt if line in {"stop", "start"}], ["stop", "start"])
            self.assertFalse(any("--force" in line for line in orb_calls_after_attempt))

            fourth = self.run_once(env)
            self.assertEqual(json.loads(fourth.stdout)["action"], "restart_suppressed_outage_latch")
            self.assertEqual(calls.read_text(encoding="utf-8").splitlines(), orb_calls_after_attempt)

            for line in docker_calls.read_text(encoding="utf-8").splitlines():
                self.assertIn("--context test-orbstack", line)

    def test_transient_docker_failure_resets_streak_after_full_recovery(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin, runtime, calls, env = self.runtime_fixture(root)
            docker = fake_bin / "docker"
            write_executable(docker, "#!/bin/sh\nexit 1\n")
            env.update(
                {
                    "CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART": "1",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_FAILURES_BEFORE_RESTART": "3",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_STARTUP_GRACE_SECS": "0",
                }
            )
            self.assertEqual(self.run_once(env).returncode, 1)

            write_executable(docker, "#!/bin/sh\necho 29.4.0\nexit 0\n")
            Path(env["FAKE_HEALTH_BODY"]).write_text('{"ok": true}\n', encoding="utf-8")
            recovered = self.run_once(env)
            self.assertEqual(recovered.returncode, 0, recovered.stderr)
            state = (runtime / "orbstack-self-heal.state").read_text(encoding="utf-8")
            self.assertIn("consecutive_docker_failures=0", state)
            self.assertIn("restart_attempted_for_outage=0", state)
            self.assertFalse(calls.exists())

    def test_parallel_triggers_are_single_flight(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin, runtime, calls, env = self.runtime_fixture(root)
            write_executable(fake_bin / "docker", "#!/bin/sh\nsleep 0.4\nexit 1\n")
            argv = ["/bin/bash", str(SELF_HEAL), "run-once", "--event", "parallel-a"]
            first = subprocess.Popen(
                argv,
                cwd=REPO_ROOT,
                env=env,
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
            lock_dir = runtime / "orbstack-self-heal.lock"
            deadline = time.monotonic() + 3
            while not lock_dir.exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue(lock_dir.exists())

            second = self.run_once(env)
            self.assertEqual(second.returncode, 0, second.stderr)
            self.assertEqual(json.loads(second.stdout)["action"], "skipped_locked")
            first_stdout, first_stderr = first.communicate(timeout=5)
            self.assertEqual(first.returncode, 1, first_stderr)
            self.assertEqual(json.loads(first_stdout)["action"], "docker_unavailable_no_restart")
            self.assertFalse(calls.exists())

    def test_health_parser_requires_root_ok_boolean(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin, _, _, env = self.runtime_fixture(root)
            write_executable(
                fake_bin / "docker",
                "#!/bin/sh\ncase \" $* \" in *\" version \"*) echo 29.4.0;; esac\nexit 0\n",
            )
            body = Path(env["FAKE_HEALTH_BODY"])
            body.write_text('{"ok": true}\n', encoding="utf-8")
            valid = self.run_once(env)
            self.assertEqual(valid.returncode, 0, valid.stderr)

            body.write_text('{"nested": {"ok": true}}\n', encoding="utf-8")
            nested = self.run_once(env)
            self.assertEqual(nested.returncode, 1)
            self.assertEqual(json.loads(nested.stdout)["health"], False)

    def test_stop_reports_bootout_failure_and_retains_plist(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            write_executable(fake_bin / "uname", "#!/bin/sh\necho Darwin\n")
            write_executable(
                fake_bin / "launchctl",
                "#!/bin/sh\nif [ \"$1\" = print ]; then exit 0; fi\nif [ \"$1\" = bootout ]; then exit 1; fi\nexit 0\n",
            )
            plist = root / "self-heal.plist"
            plist.write_text("keep", encoding="utf-8")
            env = os.environ.copy()
            env.update(
                {
                    "PATH": f"{fake_bin}:{env['PATH']}",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_RUNTIME_DIR": str(root / "runtime"),
                    "CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCHD_PLIST": str(plist),
                    "CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCHD_LABEL": "test.contextlattice.self-heal",
                }
            )
            result = subprocess.run(
                ["/bin/bash", str(SELF_HEAL), "stop"],
                cwd=REPO_ROOT,
                env=env,
                text=True,
                capture_output=True,
                check=False,
                timeout=5,
            )
            self.assertEqual(result.returncode, 1)
            payload = json.loads(result.stdout)
            self.assertFalse(payload["ok"])
            self.assertEqual(payload["error"], "bootout_failed")
            self.assertTrue(payload["plist_retained"])
            self.assertTrue(plist.exists())

    def test_global_upgrade_migrates_only_an_already_loaded_runner(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            launchctl_calls = root / "launchctl-calls"
            write_executable(fake_bin / "uname", "#!/bin/sh\necho Darwin\n")
            write_executable(
                fake_bin / "launchctl",
                f"#!/bin/sh\nprintf '%s\\n' \"$*\" >> {launchctl_calls}\nif [ \"$1\" = print ]; then exit 0; fi\nexit 0\n",
            )
            write_executable(
                fake_bin / "go",
                """#!/bin/sh
out=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = '-o' ]; then out="$2"; shift 2; continue; fi
  shift
done
[ -n "$out" ] || exit 1
mkdir -p "$(dirname "$out")"
printf '#!/bin/sh\\nexit 0\\n' > "$out"
chmod 0755 "$out"
""",
            )

            home = root / "home"
            global_home = root / "global"
            (global_home / "scripts").mkdir(parents=True)
            (global_home / "scripts" / "orbstack_self_heal.sh").write_text("legacy\n", encoding="utf-8")
            label = "test.contextlattice.self-heal"
            env = os.environ.copy()
            env.update(
                {
                    "HOME": str(home),
                    "PATH": f"{fake_bin}:{env['PATH']}",
                    "CONTEXTLATTICE_ORBSTACK_HEAL_LAUNCHD_LABEL": label,
                    "CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART": "1",
                }
            )
            result = subprocess.run(
                [
                    "/bin/bash",
                    str(INSTALLER),
                    "--global-home",
                    str(global_home),
                    "--no-shell-profile",
                    "--no-agent-hooks",
                    "--skip-venv",
                    "--quiet",
                ],
                cwd=REPO_ROOT,
                env=env,
                text=True,
                capture_output=True,
                check=False,
                timeout=30,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            installed = global_home / "scripts" / "orbstack_self_heal.sh"
            self.assertEqual(installed.read_bytes(), SELF_HEAL.read_bytes())
            plist = home / "Library" / "LaunchAgents" / f"{label}.plist"
            with plist.open("rb") as handle:
                payload = plistlib.load(handle)
            policy = payload["EnvironmentVariables"]
            self.assertEqual(policy["CONTEXTLATTICE_ORBSTACK_HEAL_VM_RESTART"], "0")
            self.assertEqual(policy["CONTEXTLATTICE_ORBSTACK_HEAL_SHED_SERVICES"], "")
            launch_calls = launchctl_calls.read_text(encoding="utf-8")
            self.assertIn("bootstrap", launch_calls)
            self.assertIn("kickstart", launch_calls)


if __name__ == "__main__":
    unittest.main()
