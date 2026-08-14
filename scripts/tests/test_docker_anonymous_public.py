from __future__ import annotations

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
WRAPPER = ROOT / "scripts/docker_anonymous_public.sh"
WINDOWS_BUILDER = ROOT / "scripts/build_windows_msi.sh"
PAID_AUDIT = ROOT / "scripts/agent/audit-paid-artifact-integrity"
IMAGE = (
    "marco98/msitools@sha256:"
    "0ac5297e0691e6768e1de4d7bdecef376ecdbff41c4cd7d4f3b55c5e7d42c48e"
)
MUTABLE_IMAGE = "marco98/msitools:" + "latest"


def write_executable(path: Path, body: str) -> None:
    path.write_text(body, encoding="utf-8")
    path.chmod(0o755)


FAKE_DOCKER = """#!/bin/sh
set -eu
printf 'argv=%s\\n' "$*" >> "$FAKE_DOCKER_LOG"
if [ "${1:-}" = "--context" ]; then
  printf '%s\\n' "$FAKE_ORBSTACK_SOCKET"
  exit 0
fi
if [ "${1:-}" != "run" ] && [ "${1:-}" != "pull" ]; then
  exit 2
fi

printf '%s\\n' "${DOCKER_CONFIG:-}" > "$FAKE_CONFIG_DIR_CAPTURE"
printf '%s\\n' "${DOCKER_HOST:-}" > "$FAKE_DOCKER_HOST_CAPTURE"
if [ -n "${DOCKER_CONTEXT+x}" ]; then
  printf '%s\\n' "$DOCKER_CONTEXT" > "$FAKE_DOCKER_CONTEXT_CAPTURE"
else
  printf '%s\\n' '<unset>' > "$FAKE_DOCKER_CONTEXT_CAPTURE"
fi
cp "$DOCKER_CONFIG/config.json" "$FAKE_CONFIG_CAPTURE"
python3 - "$DOCKER_CONFIG" "$FAKE_CONFIG_MODE_CAPTURE" <<'PY'
import os
import stat
import sys

path, output = sys.argv[1:]
with open(output, "w", encoding="ascii") as handle:
    handle.write(oct(stat.S_IMODE(os.stat(path).st_mode)))
    handle.write("\\n")
with open(output + ".json", "w", encoding="utf-8") as handle:
    handle.write(open(os.path.join(path, "config.json"), encoding="utf-8").read())
with open(output + ".config_mode", "w", encoding="ascii") as handle:
    handle.write(oct(stat.S_IMODE(os.stat(os.path.join(path, "config.json")).st_mode)))
    handle.write("\\n")
with open(output + ".entries", "w", encoding="utf-8") as handle:
    handle.write("\\n".join(sorted(os.listdir(path))))
    handle.write("\\n")
PY

if grep -q 'credsStore\\|credHelpers' "$DOCKER_CONFIG/config.json"; then
  "$FAKE_CREDENTIAL_HELPER" get
fi
exit "${FAKE_DOCKER_STATUS:-0}"
"""


class DockerAnonymousPublicTests(unittest.TestCase):
    def fixture(self) -> tuple[tempfile.TemporaryDirectory[str], dict[str, str], Path]:
        temp_root = os.environ.get("TMPDIR", "/tmp")
        temp_dir = tempfile.TemporaryDirectory(prefix="anonymous-public-", dir=temp_root)
        root = Path(temp_dir.name)
        fake_bin = root / "bin"
        fake_bin.mkdir()
        original_config = root / "original-config"
        original_config.mkdir(mode=0o700)
        original_config_file = original_config / "config.json"
        original_config_file.write_text(
            '{"auths":{},"credsStore":"fake-helper"}\n', encoding="utf-8"
        )
        original_before = original_config_file.read_bytes()
        (root / "helper-called").unlink(missing_ok=True)
        helper = fake_bin / "docker-credential-fake-helper"
        write_executable(
            helper,
            f"#!/bin/sh\nprintf 'called\\n' > '{root / 'helper-called'}'\nexit 99\n",
        )
        fake_docker = fake_bin / "docker"
        write_executable(fake_docker, FAKE_DOCKER)

        env = os.environ.copy()
        env.update(
            {
                "PATH": f"{fake_bin}:{env['PATH']}",
                "DOCKER_CONFIG": str(original_config),
                "DOCKER_CONTEXT": "unrelated-context",
                "DOCKER_HOST": "tcp://unrelated.invalid:2375",
                "TMPDIR": temp_root,
                "FAKE_ORBSTACK_SOCKET": f"unix://{root / 'orbstack.sock'}",
                "FAKE_DOCKER_LOG": str(root / "docker.log"),
                "FAKE_CONFIG_DIR_CAPTURE": str(root / "config-dir"),
                "FAKE_DOCKER_HOST_CAPTURE": str(root / "docker-host"),
                "FAKE_DOCKER_CONTEXT_CAPTURE": str(root / "docker-context"),
                "FAKE_CONFIG_CAPTURE": str(root / "config.json.capture"),
                "FAKE_CONFIG_MODE_CAPTURE": str(root / "config.mode"),
                "FAKE_CREDENTIAL_HELPER": str(helper),
                "FAKE_DOCKER_STATUS": "0",
            }
        )
        return temp_dir, env, original_before

    def run_wrapper(
        self, env: dict[str, str], *arguments: str
    ) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [str(WRAPPER), *arguments],
            cwd=ROOT,
            env=env,
            text=True,
            capture_output=True,
            check=False,
        )

    def test_success_uses_socket_before_temp_config_and_never_calls_helper(self) -> None:
        temp_dir, env, original_before = self.fixture()
        try:
            result = self.run_wrapper(
                env,
                "run",
                "--rm",
                "--platform",
                "linux/amd64",
                IMAGE,
                "wixl",
                "-v",
            )
            self.assertEqual(result.returncode, 0, result.stderr)

            log_lines = Path(env["FAKE_DOCKER_LOG"]).read_text(encoding="utf-8").splitlines()
            self.assertEqual(len(log_lines), 2)
            self.assertIn("--context orbstack context inspect orbstack", log_lines[0])
            self.assertTrue(log_lines[1].startswith("argv=run --rm --platform linux/amd64"))
            self.assertIn(IMAGE, log_lines[1])

            config_dir = Path(Path(env["FAKE_CONFIG_DIR_CAPTURE"]).read_text().strip())
            self.assertNotEqual(config_dir, Path(env["DOCKER_CONFIG"]))
            self.assertTrue(
                str(config_dir).startswith(
                    str(Path(env["TMPDIR"]) / "contextlattice-anonymous-docker.")
                )
            )
            self.assertEqual(
                json.loads(Path(env["FAKE_CONFIG_CAPTURE"]).read_text(encoding="utf-8")),
                {"auths": {}},
            )
            self.assertEqual(
                Path(env["FAKE_CONFIG_MODE_CAPTURE"] + ".entries")
                .read_text(encoding="utf-8")
                .splitlines(),
                ["config.json"],
            )
            self.assertEqual(
                Path(env["FAKE_CONFIG_MODE_CAPTURE"]).read_text(encoding="ascii").strip(),
                oct(0o700),
            )
            self.assertEqual(
                Path(env["FAKE_CONFIG_MODE_CAPTURE"] + ".config_mode")
                .read_text(encoding="ascii")
                .strip(),
                oct(0o600),
            )
            self.assertEqual(
                Path(env["FAKE_DOCKER_HOST_CAPTURE"]).read_text(encoding="utf-8").strip(),
                env["FAKE_ORBSTACK_SOCKET"],
            )
            self.assertEqual(
                Path(env["FAKE_DOCKER_CONTEXT_CAPTURE"]).read_text(encoding="utf-8").strip(),
                "<unset>",
            )
            self.assertFalse((Path(temp_dir.name) / "helper-called").exists())
            self.assertEqual(
                (Path(env["DOCKER_CONFIG"]) / "config.json").read_bytes(), original_before
            )
            self.assertFalse(config_dir.exists(), "temporary Docker config leaked after success")
        finally:
            temp_dir.cleanup()

    def test_docker_failure_cleans_config_and_preserves_global_config(self) -> None:
        temp_dir, env, original_before = self.fixture()
        try:
            env["FAKE_DOCKER_STATUS"] = "23"
            result = self.run_wrapper(env, "pull", IMAGE)
            self.assertEqual(result.returncode, 23)
            config_dir = Path(Path(env["FAKE_CONFIG_DIR_CAPTURE"]).read_text().strip())
            self.assertFalse(config_dir.exists(), "temporary Docker config leaked after failure")
            self.assertFalse((Path(temp_dir.name) / "helper-called").exists())
            self.assertEqual(
                (Path(env["DOCKER_CONFIG"]) / "config.json").read_bytes(), original_before
            )
        finally:
            temp_dir.cleanup()

    def test_rejects_credentials_private_images_and_mutable_refs_before_docker(self) -> None:
        cases = (
            ("login", "--username", "operator"),
            ("logout", "docker.io"),
            ("push", IMAGE),
            ("run", "--private", IMAGE),
            ("run", "private.example/image@sha256:" + "a" * 64),
            ("run", MUTABLE_IMAGE),
        )
        for case in cases:
            with self.subTest(case=case):
                temp_dir, env, _ = self.fixture()
                try:
                    result = self.run_wrapper(env, *case)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse(Path(env["FAKE_DOCKER_LOG"]).exists())
                    self.assertFalse((Path(temp_dir.name) / "helper-called").exists())
                finally:
                    temp_dir.cleanup()

    def test_invalid_orbstack_endpoint_fails_before_config_creation(self) -> None:
        temp_dir, env, _ = self.fixture()
        try:
            env["FAKE_ORBSTACK_SOCKET"] = "tcp://127.0.0.1:2375"
            result = self.run_wrapper(env, "run", IMAGE, "wixl")
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(Path(env["FAKE_CONFIG_DIR_CAPTURE"]).exists())
            self.assertFalse(Path(temp_dir.name, "helper-called").exists())
        finally:
            temp_dir.cleanup()

    def test_all_msitools_callers_use_digest_and_public_wrapper(self) -> None:
        source = WINDOWS_BUILDER.read_text(encoding="utf-8")
        self.assertIn(IMAGE, source)
        self.assertNotIn(MUTABLE_IMAGE, source)
        self.assertNotIn("docker run", source)
        self.assertEqual(source.count('scripts/docker_anonymous_public.sh" run'), 1)
        if PAID_AUDIT.is_file():
            audit_source = PAID_AUDIT.read_text(encoding="utf-8")
            self.assertIn(IMAGE, audit_source)
            self.assertNotIn(MUTABLE_IMAGE, audit_source)
            self.assertNotIn("docker run", audit_source)
            self.assertEqual(audit_source.count('scripts/docker_anonymous_public.sh" run'), 1)

    def test_auth_environment_is_rejected_before_docker(self) -> None:
        temp_dir, env, _ = self.fixture()
        try:
            env["DOCKER_AUTH_CONFIG"] = '{"auths":{"private.example":{}}}'
            result = self.run_wrapper(env, "run", IMAGE, "wixl")
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(Path(env["FAKE_DOCKER_LOG"]).exists())
            self.assertFalse(Path(temp_dir.name, "helper-called").exists())
        finally:
            temp_dir.cleanup()

    def test_non_orbstack_context_override_is_rejected_before_docker(self) -> None:
        temp_dir, env, _ = self.fixture()
        try:
            env["DOCKER_ANONYMOUS_PUBLIC_CONTEXT"] = "colima"
            result = self.run_wrapper(env, "run", IMAGE, "wixl")
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(Path(env["FAKE_DOCKER_LOG"]).exists())
            self.assertFalse(Path(temp_dir.name, "helper-called").exists())
        finally:
            temp_dir.cleanup()


if __name__ == "__main__":
    unittest.main()
