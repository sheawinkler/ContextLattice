#!/usr/bin/env python3
import hashlib
import json
import os
import shutil
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
INSTALLER = ROOT / "scripts/install_global_agent_tools.sh"
STAGE_CHECKPOINT = (
    'install -m 0755 "${ROOT_DIR}/scripts/agent_hooks/contextlattice_checkpoint.sh" '
    '"${LEGACY_WRAPPER_BIN_DIR}/contextlattice_checkpoint"'
)


class CheckpointHookTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.zsh = shutil.which("zsh")
        if not cls.zsh or not shutil.which("go"):
            raise unittest.SkipTest("native checkpoint launcher fixture needs zsh and Go")
        root = tempfile.TemporaryDirectory()
        cls.addClassCleanup(root.cleanup)
        cls.root = Path(root.name)
        cls.home = cls.root / "home"
        cls.tool_home = cls.home / ".contextlattice"
        cls.bin = cls.tool_home / "bin"
        cls.bin.mkdir(parents=True)
        (cls.home / ".codex").mkdir()
        (cls.root / "cwd").mkdir()
        cls.native = cls.bin / "contextlattice-agent-tools"
        subprocess.run(
            ["go", "build", "-o", str(cls.native), "./cmd/contextlattice-agent-tools"],
            cwd=ROOT / "services/gateway-go",
            check=True,
            timeout=120,
        )
        installer = INSTALLER.read_text()
        if STAGE_CHECKPOINT not in installer or "write_hook_wrapper contextlattice_checkpoint " in installer:
            raise AssertionError("checkpoint must stage the isolated launcher, not load hooks in the generic wrapper")
        stage = cls.root / "stage"
        stage.mkdir()
        subprocess.run(
            [cls.zsh, "-f", "-c", STAGE_CHECKPOINT],
            env={"PATH": os.environ["PATH"], "HOME": str(cls.home), "ROOT_DIR": str(ROOT),
                 "LEGACY_WRAPPER_BIN_DIR": str(stage)},
            check=True,
            timeout=10,
        )
        (stage / "contextlattice_checkpoint").replace(cls.bin / "contextlattice_checkpoint")

    def setUp(self):
        self.calls = []
        self.readback = b"payload\n"
        self.expected_written = b"payload\n"
        self.redirect_readback = False
        self.write_confirmed = True
        self.key = "fixture-transport-key"
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def reply(self, status, body, content_type="application/json"):
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def do_GET(self):
                fixture.calls.append(("GET", self.path, self.headers.get("X-Api-Key"), None))
                if self.path.startswith("/v1/agents/sessions/"):
                    sid = self.path.rsplit("/", 1)[-1]
                    actor, native_id = {
                        "sess-mercury": ("mercury_agent", "native-mercury"),
                        "sess-omp": ("omp_agent", "native-omp"),
                    }.get(sid, ("codex_gpt5", "default-native"))
                    self.reply(200, json.dumps({"ok": True, "session": {
                        "id": sid, "agent_id": actor, "project": "p", "status": "active",
                        "native_session_id": native_id,
                    }}).encode())
                elif self.path.startswith("/memory/files/p/"):
                    if fixture.redirect_readback:
                        self.send_response(302)
                        self.send_header("Location", "/memory/files/p/other.md")
                        self.end_headers()
                    else:
                        self.reply(200, fixture.readback, "text/plain")
                else:
                    self.reply(404, b"{}")

            def do_POST(self):
                data = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                fixture.calls.append(("POST", self.path, self.headers.get("X-Api-Key"), data))
                digest = hashlib.sha256(fixture.expected_written).hexdigest()
                receipt = {"source": "fixture", "content_ref": "sha256:" + digest}
                if fixture.write_confirmed:
                    receipt["ok"] = True
                self.reply(200, json.dumps(receipt).encode())

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(thread.join, 5)
        self.addCleanup(server.shutdown)
        self.url = f"http://127.0.0.1:{server.server_address[1]}"
        self.inject_defaults(self.url)

    def inject_defaults(self, url):
        defaults = {
            "CONTEXTLATTICE_ORCHESTRATOR_URL": url,
            "CONTEXTLATTICE_ORCHESTRATOR_API_KEY": self.key,
            "CONTEXTLATTICE_AGENT": "codex",
            "CONTEXTLATTICE_AGENT_ID": "codex_gpt5",
            "MEMMCP_AGENT_ID": "codex_gpt5",
            "CONTEXTLATTICE_MODEL": "invented-model",
            "CONTEXTLATTICE_AGENT_MODEL": "invented-alias",
            "CONTEXTLATTICE_SESSION_ID": "default-session",
            "CONTEXTLATTICE_NATIVE_SESSION_ID": "default-native",
        }
        (self.tool_home / "agent_hooks.env").write_text(
            "".join(f"export {name}='{value}'\n" for name, value in defaults.items())
        )
        (self.home / ".codex/contextlattice_hooks.env").write_text(
            f"export CONTEXTLATTICE_GLOBAL_HOME='{self.root / 'wrong-home'}'\n"
            "export CONTEXTLATTICE_AGENT_ID='second-invented-actor'\n"
            "export CONTEXTLATTICE_MODEL='second-invented-model'\n"
            "export CONTEXTLATTICE_NATIVE_SESSION_ID='second-invented-native'\n"
        )

    def run_hook(self, env=None, args=None, stdin=None):
        return subprocess.run(
            [self.zsh, "-f", str(self.bin / "contextlattice_checkpoint"),
             "-p", "p", "-t", "t", "-f", "notes/sub/target #.md",
             *(args or ["-c", "payload", "--query", "ignored query"])],
            cwd=self.root / "cwd",
            env={"PATH": os.environ["PATH"], "HOME": str(self.home), **(env or {})},
            input=stdin, text=True, capture_output=True, timeout=15, check=False,
        )

    def test_explicit_actor_native_session_model_and_transport_win(self):
        self.inject_defaults("http://127.0.0.1:1")
        run = self.run_hook({
            "MEMMCP_ORCHESTRATOR_URL": self.url,
            "CONTEXTLATTICE_ORCHESTRATOR_API_KEY": "fixture-explicit-key",
            "CONTEXTLATTICE_AGENT_ID": "mercury_agent",
            "MEMMCP_AGENT_ID": "other-actor",
            "CONTEXTLATTICE_MODEL": "served-model",
            "CONTEXTLATTICE_SESSION_ID": "sess-mercury",
            "CONTEXTLATTICE_NATIVE_SESSION_ID": "native-mercury",
        })
        self.assertEqual(run.returncode, 0, run.stderr)
        self.assertEqual([call[:2] for call in self.calls], [
            ("GET", "/v1/agents/sessions/sess-mercury"),
            ("POST", "/memory/write"),
            ("GET", "/memory/files/p/notes/sub/target%20%23.md"),
        ])
        self.assertTrue(all(call[2] == "fixture-explicit-key" for call in self.calls))
        write = self.calls[1][3]
        self.assertEqual((write["agent_id"], write["session_id"]), ("mercury_agent", "sess-mercury"))
        self.assertIn("mercury_agent-served-model", write["tags"])
        self.assertEqual(json.loads(run.stdout)["readback"]["first_file"], "notes/sub/target #.md")

    def test_unset_actor_and_model_do_not_inherit_defaults(self):
        run = self.run_hook()
        self.assertEqual(run.returncode, 0, run.stderr)
        self.assertEqual(len(self.calls), 2)
        self.assertEqual(self.calls[0][2], self.key)
        write = self.calls[0][3]
        self.assertEqual((write["agent_id"], write["session_id"]), ("agent-cli", None))
        self.assertEqual(write["tags"], ["caller:agent-cli"])

    def test_legacy_actor_alias_and_unknown_model_are_preserved(self):
        run = self.run_hook({
            "CONTEXTLATTICE_AGENT_ID": "",
            "MEMMCP_AGENT_ID": "omp_agent",
            "CONTEXTLATTICE_MODEL": "unknown",
            "CONTEXTLATTICE_AGENT_MODEL": "other-model",
            "CONTEXTLATTICE_SESSION_ID": "sess-omp",
            "CONTEXTLATTICE_NATIVE_SESSION_ID": "native-omp",
        }, ["--stdin"], "payload\n\n")
        self.assertEqual(run.returncode, 0, run.stderr)
        write = self.calls[1][3]
        self.assertEqual((write["agent_id"], write["session_id"]), ("omp_agent", "sess-omp"))
        self.assertEqual(write["tags"], ["caller:omp_agent"])

    def test_mismatched_or_missing_caller_cannot_attach_foreign_session(self):
        for env in ({}, {"CONTEXTLATTICE_AGENT_ID": "omp_agent"}, {
            "CONTEXTLATTICE_AGENT_ID": "mercury_agent", "CONTEXTLATTICE_NATIVE_SESSION_ID": "wrong-native",
        }):
            with self.subTest(env=env):
                self.calls.clear()
                run = self.run_hook({"CONTEXTLATTICE_SESSION_ID": "sess-mercury", **env})
                self.assertNotEqual(run.returncode, 0, run.stdout)
                self.assertEqual([call[:2] for call in self.calls], [("GET", "/v1/agents/sessions/sess-mercury")])

    def test_readback_rejects_mismatched_content_and_digest(self):
        for body in (b"prefix payload suffix", b"payload\n\n"):
            with self.subTest(body=body):
                self.readback = body
                run = self.run_hook()
                self.assertNotEqual(run.returncode, 0, run.stderr)
                receipt = json.loads(run.stdout)
                self.assertFalse(receipt["ok"])
                self.assertTrue(receipt["write"]["ok"])
                self.assertEqual(receipt["readback"]["count"], 0)

    def test_readback_never_follows_a_redirect(self):
        self.redirect_readback = True
        run = self.run_hook()
        self.assertNotEqual(run.returncode, 0)
        self.assertEqual(len(self.calls), 2)

    def test_unconfirmed_write_does_not_read_back(self):
        self.write_confirmed = False
        run = self.run_hook()
        self.assertNotEqual(run.returncode, 0)
        self.assertEqual([call[:2] for call in self.calls], [("POST", "/memory/write")])

    def test_missing_native_binary_has_no_legacy_write_fallback(self):
        run = self.run_hook({"CONTEXTLATTICE_GLOBAL_HOME": str(self.root / "missing-native")})
        self.assertNotEqual(run.returncode, 0)
        self.assertEqual(self.calls, [])

    def test_content_file_wins_and_legacy_query_is_ignored(self):
        content_file = self.root / "checkpoint.txt"
        content_file.write_text("payload\n")
        run = self.run_hook(
            env={"CONTEXTLATTICE_CLI_OUTPUT": "pretty"},
            args=["--stdin", "--content-file", str(content_file), "-q", "old search"],
            stdin="wrong content",
        )
        self.assertEqual(run.returncode, 0, run.stderr)
        self.assertEqual(self.calls[0][3]["content"], "payload")
        self.assertEqual(len(self.calls), 2)
        self.assertEqual(len(run.stdout.splitlines()), 1)


class CheckpointWindowsContractTests(unittest.TestCase):
    def test_powershell_installer_names_the_native_checkpoint_command(self):
        installer = (ROOT / "scripts/install_global_agent_tools.ps1").read_text()
        commands = installer.split("$goNativeCommands = @(", 1)[1].split("\n)", 1)[0]
        self.assertIn('"contextlattice_checkpoint"', commands)
        self.assertIn('"%GO_TOOL%" $Name %*', installer)


if __name__ == "__main__":
    unittest.main()
