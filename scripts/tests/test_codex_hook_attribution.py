import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
NATIVE = "11111111-2222-4333-8444-555555555555"
OTHER = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
HOOKS = {
    "SessionStart": "agent_start.sh",
    "PreCompact": "contextlattice_pre_compaction_write.sh",
    "PostCompact": "contextlattice_post_compaction_read.sh",
}


@unittest.skipUnless(os.name == "posix" and Path("/bin/zsh").is_file(), "Unix Codex hook fixture requires zsh")
class CodexHookAttributionTests(unittest.TestCase):
    def setUp(self):
        scratch = tempfile.TemporaryDirectory()
        self.addCleanup(scratch.cleanup)
        self.root = Path(scratch.name)
        home = self.root / "home"
        home.mkdir()
        bindir = self.root / "bin"
        bindir.mkdir()
        hooks = self.root / "scripts/agent_hooks"
        hooks.mkdir(parents=True)
        agent = self.root / "scripts/agent"
        agent.mkdir()
        for name in (*HOOKS.values(), "common.sh"):
            source = (ROOT / "scripts/agent_hooks" / name).read_text()
            self.executable(hooks / name, "#!/bin/zsh -f\n" + source.split("\n", 1)[1])
        for name in ("resource_pressure_guard.sh", "git_lane_guard.sh", "orbstack_forward_guard.sh",
                     "native_endpoint_smoke.sh", "recall_monitor_seed.sh", "agent_policy_pack.sh"):
            self.executable(hooks / name, '#!/bin/zsh -f\nprintf "%s\\n" \'{"ok":true}\'\n')
        self.executable(agent / "audit-codex-session-store", '#!/bin/zsh -f\nprintf "%s\\n" \'{"ok":true}\'\n')
        for name in ("_common.py", "compaction-handoff-payload", "contextlattice-session"):
            shutil.copyfile(ROOT / "scripts/agent" / name, agent / name)
        (self.root / "config/agents").mkdir(parents=True)
        (self.root / "config/agents/agent_profiles.json").write_text("{}")
        (self.root / "scripts/agent_orchestration.py").write_text(
            'import os, pathlib, sys\n'
            'pathlib.Path(os.environ["HANDOFF"]).write_text(sys.argv[3])\n'
            'print(\'{"ok":true}\')\n'
        )
        self.executable(bindir / "contextlattice", f"#!{sys.executable}\n" + """
import json, os, pathlib, sys, time
pathlib.Path(os.environ['IDENTITY_ARGS']).write_text(json.dumps(sys.argv[1:]))
mode = os.getenv('IDENTITY_MODE', 'known')
if mode == 'old':
    sys.exit(2)
if mode == 'slow':
    time.sleep(10)
if mode == 'malformed':
    print('not-json')
    sys.exit()
print(json.dumps({'agent_id': 'not-the-owner', 'caller_attribution': {
    'source': 'caller_reported', 'harness': 'codex', 'model_known': True,
    'model': 'other-2' if mode == 'foreign_model' else sys.argv[sys.argv.index('--model') + 1],
    'model_origin': 'fixture', 'model_family': 'family', 'model_version': '2', 'reasoning_effort': 'high',
    'execution_label': 'fabricated', 'origin': 'legacy', 'harness_origin': 'legacy', 'effort': 'max'
}}))
""")
        self.calls = []
        self.rows = {}
        self.wrong_native = False
        fixture = self

        class Handler(BaseHTTPRequestHandler):
            def respond(self, body):
                raw = json.dumps(body).encode()
                self.send_response(200)
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_GET(self):
                fixture.calls.append((self.path, None))
                row = fixture.rows.get(self.path.rsplit("/", 1)[-1])
                self.respond({"ok": row is not None, "session": row})

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                fixture.calls.append((self.path, body))
                if self.path == "/memory/search":
                    self.respond({"results": [{"file": "fixture.md"}]})
                    return
                sid = body.get("session_id") or next((
                    key for key, row in fixture.rows.items()
                    if row["project"] == body["project"] and row["agent_id"] == body["agent_id"]
                    and row["native_session_id"] == body.get("native_session_id")
                    and (not body.get("agent") or row["agent"] == body["agent"])
                ), f"sess_{body.get('native_session_id', 'legacy')}_{len(fixture.rows)}")
                row = fixture.rows.setdefault(sid, {
                    "id": sid, "project": body["project"], "agent": body.get("agent", ""),
                    "agent_id": body["agent_id"], "native_session_id": body.get("native_session_id"),
                    "reuse_key": body.get("reuse_key"), "status": "active",
                })
                facts = body["metadata"].get("caller_attribution", {})
                row["execution_identity"] = {**facts} if facts.get("model_known") is True else {
                    key: value for key, value in facts.items() if key != "model"
                }
                self.respond({"ok": True, "session": {**row, "native_session_id": OTHER}
                              if fixture.wrong_native else row})

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.addCleanup(server.server_close)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(server.shutdown)
        self.env = {
            "HOME": str(home), "PATH": os.pathsep.join((str(bindir), os.environ["PATH"])),
            "TMPDIR": scratch.name, "PYTHONDONTWRITEBYTECODE": "1",
            "CONTEXTLATTICE_REPO_ROOT": scratch.name, "CONTEXTLATTICE_AGENT_ID": "fixture-owner",
            "CONTEXTLATTICE_AGENT": "legacy-compact-owner", "CODEX_SESSION_ID": OTHER,
            "CONTEXTLATTICE_PROJECT": "fixture", "CONTEXTLATTICE_HOOK_PROJECT": "fixture",
            "CONTEXTLATTICE_ORCHESTRATOR_URL": f"http://127.0.0.1:{server.server_port}",
            "CONTEXTLATTICE_ORCHESTRATOR_API_KEY": "fixture-key",
            "HANDOFF": str(self.root / "handoff.json"), "IDENTITY_ARGS": str(self.root / "identity.json"),
        }

    @staticmethod
    def executable(path, contents):
        path.write_text(contents)
        path.chmod(0o700)

    def command(self, event, wrapper=True):
        hook = HOOKS[event]
        if wrapper:
            name = "contextlattice_agent_start.sh" if event == "SessionStart" else hook
            path = ROOT / "config/codex" / name
        else:
            path = self.root / "scripts/agent_hooks" / hook
        return ["/bin/zsh", "-f", str(path)]

    def run_hook(self, event, raw=None, wrapper=True, **env):
        run = subprocess.run(self.command(event, wrapper), cwd=self.root, env={**self.env, **env},
                             input=raw if raw is not None else self.event(event),
                             capture_output=True, timeout=6)
        self.assertEqual(run.returncode, 0, run.stderr.decode(errors="replace"))
        result = json.loads(run.stdout)
        self.assertTrue(result["continue"])
        self.assertEqual(list(self.root.glob("contextlattice-codex-hook.*")), [])
        return result

    def test_session_start_emits_bounded_host_output_not_internal_diagnostics(self):
        helper = self.root / "scripts/agent_hooks/agent_start.sh"
        self.executable(helper, "#!/bin/zsh -f\nprintf '%s\\n' 'internal diagnostic'\nexit 2\n")
        degraded = self.run_hook("SessionStart")
        self.assertIn("systemMessage", degraded)
        self.assertNotIn("internal diagnostic", json.dumps(degraded))
        self.executable(helper, "#!/bin/zsh -f\nprintf '%s\\n' '{\"ok\":true,\"retrieval\":{\"top_results\":[{\"file\":\"a.md\",\"debug\":\"ignored\"}]}}'\n")
        detail = self.run_hook("SessionStart")["hookSpecificOutput"]
        self.assertEqual(detail["hookEventName"], "SessionStart")
        self.assertEqual(json.loads(detail["additionalContext"])["top_results"], [{"file": "a.md"}])

    @staticmethod
    def event(event, **fields):
        return json.dumps({"hook_event_name": event, "session_id": NATIVE, "model": "fixture-2",
                           "transcript_path": "/not/read/transcript.jsonl", "cwd": "/not/trusted",
                           **fields}).encode()

    def starts(self):
        return [body for path, body in self.calls if path == "/v1/agents/sessions/start"]

    def test_all_wrappers_bind_one_native_session_and_keep_compact_input(self):
        self.run_hook("SessionStart", CODEX_THREAD_ID=NATIVE)
        self.run_hook("PreCompact", self.event("PreCompact", summary="pre-handoff"),
                      CODEX_THREAD_ID=OTHER)
        self.run_hook("PostCompact", self.event("PostCompact", model="fixture-3", summary="post-handoff"),
                      CODEX_THREAD_ID=NATIVE)
        starts = self.starts()
        self.assertEqual(len(starts), 3)
        self.assertEqual(len(self.rows), 1)
        self.assertEqual([p["native_session_id"] for p in starts], [NATIVE] * 3)
        self.assertEqual([p["agent_id"] for p in starts], ["fixture-owner"] * 3)
        self.assertTrue(all("agent" not in p and "reuse_key" not in p for p in starts))
        attrs = [p["metadata"]["caller_attribution"] for p in starts]
        self.assertEqual([a["model"] for a in attrs], ["fixture-2", "fixture-2", "fixture-3"])
        self.assertEqual(attrs[0]["reasoning_effort"], "high")
        self.assertNotIn("reasoning_effort", attrs[1])
        self.assertFalse({"execution_label", "origin", "harness_origin", "effort"} & attrs[0].keys())
        self.assertEqual(attrs[2]["model_origin"], "fixture")
        self.assertEqual(attrs[2]["model_family"], "family")
        self.assertEqual(json.loads((self.root / "identity.json").read_text()),
                         ["identity", "--agent", "codex", "--model", "fixture-3", "--native-session-id", NATIVE, "--raw"])
        handoff = json.loads((self.root / "handoff.json").read_text())
        self.assertEqual((handoff["session_id"], handoff["summary"]), (NATIVE, "pre-handoff"))
        search = self.calls[-1][1]
        self.assertIn("post-handoff", search["query"])
        self.assertEqual(search["session_id"], f"sess_{NATIVE}_0")

    def test_missing_invalid_and_slow_resolver_are_partial(self):
        for mode in ("old", "malformed", "slow", "foreign_model"):
            with self.subTest(mode=mode):
                self.run_hook("PostCompact", self.event("PostCompact", model="  openai/fixture:2\t"),
                              IDENTITY_MODE=mode, CODEX_THREAD_ID=NATIVE)
                self.assertEqual(self.starts()[-1]["metadata"]["caller_attribution"],
                                 {"source": "caller_reported", "harness": "codex",
                                  "model": "openai/fixture:2", "model_known": True})
                self.assertEqual(self.rows[f"sess_{NATIVE}_0"]["execution_identity"]["model"],
                                 "openai/fixture:2")

    def test_installed_resolver_when_hook_path_has_no_cli(self):
        installed_bin = self.root / "home/.contextlattice/bin"
        installed_bin.mkdir(parents=True)
        (self.root / "bin/contextlattice").rename(installed_bin / "contextlattice")
        self.run_hook("SessionStart", PATH=f"{Path(sys.executable).parent}:/usr/bin:/bin")
        self.assertEqual(self.starts()[0]["metadata"]["caller_attribution"]["model_family"], "family")

    def test_invalid_host_input_never_creates_legacy_session(self):
        for raw, env in ((b"", {}), (b"{", {}), (self.event("PreCompact"), {}),
                         (self.event("PostCompact", session_id="nonsense"), {}),
                         (self.event("PostCompact"), {"CONTEXTLATTICE_COMPACTION_STDIN_MAX_BYTES": "10"})):
            with self.subTest(raw=raw):
                self.run_hook("PostCompact", raw, CONTEXTLATTICE_SESSION_ID="sess_foreign", **env)
        self.assertEqual(self.starts(), [])
        self.assertTrue(all(body["session_id"] == "" for _, body in self.calls))
        self.assertFalse((self.root / "identity.json").exists())

    def test_missing_model_and_disabled_ensure_do_not_infer_identity(self):
        for model in ("", " UNKNOWN\t", "default", "auto", "none", "null", "N/A",
                      "/absolute", "https://route", "bad+model", "x" * 121):
            self.run_hook("SessionStart", self.event("SessionStart", model=model))
            self.assertEqual(self.starts()[-1]["metadata"]["caller_attribution"],
                             {"source": "caller_reported", "harness": "codex", "model_known": False})
        self.assertFalse((self.root / "identity.json").exists())
        self.run_hook("PostCompact", CONTEXTLATTICE_AUTO_SESSION_DISABLED="1",
                      CONTEXTLATTICE_SESSION_ID="sess_foreign")
        self.assertEqual(len(self.starts()), 11)
        self.assertEqual(self.calls[-1][1]["session_id"], "")

    def test_unpinned_native_ensure_reuses_different_display_alias(self):
        self.rows["sess_preexisting"] = {
            "id": "sess_preexisting", "project": "fixture", "agent": "different-display-alias",
            "agent_id": "fixture-owner", "native_session_id": NATIVE, "status": "active",
        }
        self.run_hook("PostCompact", IDENTITY_MODE="old")
        self.assertEqual(list(self.rows), ["sess_preexisting"])
        self.assertEqual(self.rows["sess_preexisting"]["agent"], "different-display-alias")
        self.assertEqual(self.rows["sess_preexisting"]["execution_identity"]["model"], "fixture-2")
        self.assertEqual(self.calls[-1][1]["session_id"], "sess_preexisting")

    def test_bad_compact_event_preserves_summary_without_foreign_handoff_id(self):
        self.run_hook("PreCompact", self.event("PostCompact", summary="text to preserve"))
        handoff = json.loads((self.root / "handoff.json").read_text())
        self.assertEqual((handoff["session_id"], handoff["summary"]), ("", "text to preserve"))
        self.assertEqual(self.starts(), [])

    def test_gateway_bind_checks_inherited_and_returned_native_identity(self):
        self.run_hook("SessionStart")
        inherited = f"sess_{NATIVE}_0"
        self.run_hook("PostCompact", CONTEXTLATTICE_SESSION_ID=inherited)
        self.assertEqual(self.starts()[-1]["session_id"], inherited)
        self.assertEqual(self.calls[-1][1]["session_id"], inherited)
        self.run_hook("PostCompact", CONTEXTLATTICE_SESSION_ID="sess_foreign")
        self.assertEqual(len(self.starts()), 2)
        self.assertEqual(self.calls[-1][1]["session_id"], "")
        self.wrong_native = True
        self.run_hook("PostCompact")
        self.assertEqual(len(self.starts()), 3)
        self.assertEqual(self.calls[-1][1]["session_id"], "")

    def test_manual_compaction_keeps_legacy_ensure_and_input(self):
        self.run_hook("PreCompact", b'{"summary":"manual handoff"}', wrapper=False)
        start, = self.starts()
        self.assertNotIn("native_session_id", start)
        self.assertEqual(start["agent"], "legacy-compact-owner")
        self.assertEqual(json.loads((self.root / "handoff.json").read_text())["summary"], "manual handoff")

    def test_open_pipe_and_terminal_do_not_block(self):
        import pty
        for child_fd, peer_fd in (os.pipe(), tuple(reversed(pty.openpty()))):
            try:
                run = subprocess.run(self.command("PostCompact"), cwd=self.root, env=self.env,
                                     stdin=child_fd, capture_output=True, check=True, timeout=3)
                self.assertTrue(json.loads(run.stdout)["continue"])
            finally:
                os.close(child_fd)
                os.close(peer_fd)
        self.assertEqual(self.starts(), [])
        self.assertEqual(list(self.root.glob("contextlattice-codex-hook.*")), [])


if __name__ == "__main__":
    unittest.main()
