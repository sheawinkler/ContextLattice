from __future__ import annotations

import hashlib
import json
import os
import plistlib
import shutil
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
INSTALLER = ROOT / "scripts/install_retention_runner.sh"
SHELL = Path("/bin/zsh") if Path("/bin/zsh").is_file() else Path(shutil.which("bash") or "/bin/sh")
SOURCE_COMMIT = "a" * 40
SOURCE_TREE = "b" * 40


FAKE_LAUNCHCTL = r"""
#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import plistlib
import sys
from pathlib import Path


state_path = Path(os.environ["FAKE_LAUNCHCTL_STATE"])
log_path = Path(os.environ["FAKE_LAUNCHCTL_LOG"])
state = json.loads(state_path.read_text(encoding="utf-8")) if state_path.exists() else {}
args = sys.argv[1:]
with log_path.open("a", encoding="utf-8") as handle:
    handle.write(json.dumps(args) + "\n")


def save() -> None:
    state_path.write_text(json.dumps(state, sort_keys=True), encoding="utf-8")


def target_label(value: str) -> str:
    return value.rsplit("/", 1)[-1]


command = args[0] if args else ""
if command == "print":
    label = target_label(args[-1])
    if label not in state:
        raise SystemExit(1)
    print(f"label = {label}")
    print(f"plist = {state[label]}")
elif command == "bootout":
    label = target_label(args[-1])
    if os.environ.get("FAKE_LAUNCHCTL_FAIL_BOOTOUT_LABEL") == label:
        raise SystemExit(3)
    state.pop(label, None)
    save()
elif command == "bootstrap":
    plist_path = Path(args[-1])
    with plist_path.open("rb") as handle:
        payload = plistlib.load(handle)
    env = payload.get("EnvironmentVariables", {})
    if env.get("CONTEXTLATTICE_RETENTION_SOURCE_COMMIT") == os.environ.get("FAKE_LAUNCHCTL_FAIL_BOOTSTRAP_COMMIT"):
        raise SystemExit(5)
    state[str(payload["Label"])] = str(plist_path)
    save()
elif command == "enable":
    label = target_label(args[-1])
    if os.environ.get("FAKE_LAUNCHCTL_FAIL_ENABLE_LABEL") == label:
        raise SystemExit(6)
elif command == "kickstart":
    label = target_label(args[-1])
    if os.environ.get("FAKE_LAUNCHCTL_FAIL_KICKSTART_LABEL") == label:
        raise SystemExit(7)
elif command == "list":
    for label in sorted(state):
        print(f"-\t0\t{label}")
else:
    raise SystemExit(2)
"""


def plist_payload(label: str, runner: Path, *, commit: str = "c" * 40) -> dict[str, object]:
    return {
        "Label": label,
        "ProgramArguments": [str(runner)],
        "EnvironmentVariables": {
            "CONTEXTLATTICE_RETENTION_SOURCE_COMMIT": commit,
            "CONTEXTLATTICE_RETENTION_SOURCE_TREE": "d" * 40,
        },
    }


class RetentionRunnerInstallTests(unittest.TestCase):
    def setUp(self) -> None:
        self.temp_dir = tempfile.TemporaryDirectory(prefix="contextlattice-retention-install-")
        self.root = Path(self.temp_dir.name)
        self.launch_agents = self.root / "LaunchAgents"
        self.logs = self.root / "logs"
        self.state_path = self.root / "launchctl-state.json"
        self.log_path = self.root / "launchctl-calls.jsonl"
        self.launchctl = self.root / "launchctl"
        self.launchctl.write_text(textwrap.dedent(FAKE_LAUNCHCTL).lstrip(), encoding="utf-8")
        self.launchctl.chmod(0o755)
        self.runner = self.root / "retention-runner"
        self.runner.write_text("#!/bin/zsh -f\nexit 0\n", encoding="utf-8")
        self.runner.chmod(0o755)
        self.label = "io.contextlattice.retention.test"
        self.legacy_label = "io.contextlattice.retention.legacy.test"

    def tearDown(self) -> None:
        self.temp_dir.cleanup()

    def env(self, **overrides: str) -> dict[str, str]:
        env = os.environ.copy()
        env.update(
            {
                "CONTEXTLATTICE_RETENTION_LAUNCHD_LABEL": self.label,
                "CONTEXTLATTICE_RETENTION_LEGACY_LAUNCHD_LABEL": self.legacy_label,
                "CONTEXTLATTICE_RETENTION_LAUNCH_AGENTS_DIR": str(self.launch_agents),
                "CONTEXTLATTICE_RETENTION_LOG_DIR": str(self.logs),
                "CONTEXTLATTICE_RETENTION_RUNNER_PATH": str(self.runner),
                "CONTEXTLATTICE_RETENTION_WORKING_DIRECTORY": str(self.root),
                "CONTEXTLATTICE_RETENTION_LAUNCHCTL": str(self.launchctl),
                "CONTEXTLATTICE_RETENTION_SOURCE_COMMIT": SOURCE_COMMIT,
                "CONTEXTLATTICE_RETENTION_SOURCE_TREE": SOURCE_TREE,
                "FAKE_LAUNCHCTL_STATE": str(self.state_path),
                "FAKE_LAUNCHCTL_LOG": str(self.log_path),
            }
        )
        env.update(overrides)
        return env

    def run_installer(self, action: str, *, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
        argv = [str(SHELL)]
        if SHELL.name == "zsh":
            argv.append("-f")
        argv.extend([str(INSTALLER), action])
        return subprocess.run(
            argv,
            cwd=ROOT,
            capture_output=True,
            check=False,
            env=env or self.env(),
            text=True,
            timeout=20,
        )

    def write_state(self, payload: dict[str, str]) -> None:
        self.state_path.write_text(json.dumps(payload, sort_keys=True), encoding="utf-8")

    def read_state(self) -> dict[str, str]:
        return json.loads(self.state_path.read_text(encoding="utf-8")) if self.state_path.exists() else {}

    def test_install_uses_daily_default_and_source_bound_isolated_identity(self) -> None:
        self.launch_agents.mkdir(parents=True)
        legacy_plist = self.launch_agents / f"{self.legacy_label}.plist"
        with legacy_plist.open("wb") as handle:
            plistlib.dump(plist_payload(self.legacy_label, self.runner), handle)
        self.write_state({self.legacy_label: str(legacy_plist)})

        result = self.run_installer("install", env=self.env(RETENTION_RUN_AT_LOAD="1"))

        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        plist_path = self.launch_agents / f"{self.label}.plist"
        with plist_path.open("rb") as handle:
            payload = plistlib.load(handle)
        self.assertEqual(payload["Label"], self.label)
        self.assertEqual(payload["StartInterval"], 86400)
        self.assertEqual(payload["ProgramArguments"], [str(self.runner)])
        self.assertTrue(payload["RunAtLoad"])
        installed_env = payload["EnvironmentVariables"]
        self.assertEqual(installed_env["CONTEXTLATTICE_RETENTION_SOURCE_COMMIT"], SOURCE_COMMIT)
        self.assertEqual(installed_env["CONTEXTLATTICE_RETENTION_SOURCE_TREE"], SOURCE_TREE)
        self.assertEqual(
            installed_env["CONTEXTLATTICE_RETENTION_RUNNER_SHA256"],
            hashlib.sha256(self.runner.read_bytes()).hexdigest(),
        )
        self.assertFalse(legacy_plist.exists())
        self.assertEqual(self.read_state(), {self.label: str(plist_path)})
        calls = [json.loads(line) for line in self.log_path.read_text(encoding="utf-8").splitlines()]
        self.assertIn(["kickstart", "-k", f"gui/{os.getuid()}/{self.label}"], calls)
        self.assertNotIn("com.sheawinkler.contextlattice-retention", self.log_path.read_text(encoding="utf-8"))

    def test_legacy_bootout_failure_retains_legacy_plist_and_blocks_install(self) -> None:
        self.launch_agents.mkdir(parents=True)
        legacy_plist = self.launch_agents / f"{self.legacy_label}.plist"
        with legacy_plist.open("wb") as handle:
            plistlib.dump(plist_payload(self.legacy_label, self.runner), handle)
        self.write_state({self.legacy_label: str(legacy_plist)})

        result = self.run_installer(
            "install",
            env=self.env(FAKE_LAUNCHCTL_FAIL_BOOTOUT_LABEL=self.legacy_label),
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("could not unload", result.stderr)
        self.assertTrue(legacy_plist.exists())
        self.assertFalse((self.launch_agents / f"{self.label}.plist").exists())
        self.assertIn(self.legacy_label, self.read_state())

    def test_uninstall_bootout_failure_retains_current_plist(self) -> None:
        self.launch_agents.mkdir(parents=True)
        plist_path = self.launch_agents / f"{self.label}.plist"
        with plist_path.open("wb") as handle:
            plistlib.dump(plist_payload(self.label, self.runner), handle)
        self.write_state({self.label: str(plist_path)})

        result = self.run_installer(
            "uninstall",
            env=self.env(FAKE_LAUNCHCTL_FAIL_BOOTOUT_LABEL=self.label),
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(plist_path.exists())
        self.assertIn(self.label, self.read_state())

    def test_failed_replacement_restores_prior_loaded_plist(self) -> None:
        self.launch_agents.mkdir(parents=True)
        plist_path = self.launch_agents / f"{self.label}.plist"
        old_payload = plist_payload(self.label, self.runner)
        with plist_path.open("wb") as handle:
            plistlib.dump(old_payload, handle)
        old_bytes = plist_path.read_bytes()
        self.write_state({self.label: str(plist_path)})

        result = self.run_installer(
            "install",
            env=self.env(FAKE_LAUNCHCTL_FAIL_BOOTSTRAP_COMMIT=SOURCE_COMMIT),
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("prior LaunchAgent state restored", result.stderr)
        self.assertEqual(plist_path.read_bytes(), old_bytes)
        self.assertEqual(self.read_state(), {self.label: str(plist_path)})

    def test_failed_rollback_unload_retains_prior_plist_backup(self) -> None:
        self.launch_agents.mkdir(parents=True)
        plist_path = self.launch_agents / f"{self.label}.plist"
        old_payload = plist_payload(self.label, self.runner)
        with plist_path.open("wb") as handle:
            plistlib.dump(old_payload, handle)
        old_bytes = plist_path.read_bytes()

        result = self.run_installer(
            "install",
            env=self.env(
                FAKE_LAUNCHCTL_FAIL_ENABLE_LABEL=self.label,
                FAKE_LAUNCHCTL_FAIL_BOOTOUT_LABEL=self.label,
            ),
        )

        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Prior plist backup retained", result.stderr)
        backups = list(self.launch_agents.glob(f"{self.label}.plist.backup.*"))
        self.assertEqual(len(backups), 1)
        self.assertEqual(backups[0].read_bytes(), old_bytes)
        self.assertIn(self.label, self.read_state())


if __name__ == "__main__":
    unittest.main()
