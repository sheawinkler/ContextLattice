"""Unit tests for the Hermes ContextLattice memory provider.

Runs without Hermes installed: `agent.memory_provider` is stubbed and ContextLattice is a
local fake HTTP server. Run: python3 -m unittest discover integrations/hermes-memory-provider/tests
"""
from __future__ import annotations

import importlib.util
import hashlib
import json
import os
import sys
import tempfile
import threading
import types
import unittest
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest.mock import patch

PLUGIN = Path(__file__).resolve().parents[1] / "__init__.py"


def _load_plugin():
    agent = types.ModuleType("agent")
    mp = types.ModuleType("agent.memory_provider")

    class MemoryProvider:  # minimal ABC stand-in
        pass

    def spawn_context_thread(target, *, name, daemon=True, args=(), kwargs=None):
        return threading.Thread(target=target, args=args, kwargs=kwargs or {}, name=name, daemon=daemon)

    mp.MemoryProvider = MemoryProvider
    mp.spawn_context_thread = spawn_context_thread
    sys.modules.setdefault("agent", agent)
    sys.modules["agent.memory_provider"] = mp
    spec = importlib.util.spec_from_file_location("cl_hermes_plugin_under_test", PLUGIN)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


plugin = _load_plugin()
_MISSING = object()


@contextmanager
def _secret_scope_module(resolver=_MISSING):
    name = "agent.secret_scope"
    parent = sys.modules.get("agent")
    previous_module = sys.modules.pop(name, _MISSING)
    previous_attr = getattr(parent, "secret_scope", _MISSING) if parent else _MISSING
    if resolver is not _MISSING:
        module = types.ModuleType(name)
        if resolver is not None:
            module.get_secret = resolver
        sys.modules[name] = module
        if parent:
            parent.secret_scope = module
    elif parent and hasattr(parent, "secret_scope"):
        delattr(parent, "secret_scope")
    try:
        yield
    finally:
        sys.modules.pop(name, None)
        if previous_module is not _MISSING:
            sys.modules[name] = previous_module
        if parent:
            if previous_attr is _MISSING:
                if hasattr(parent, "secret_scope"):
                    delattr(parent, "secret_scope")
            else:
                parent.secret_scope = previous_attr


class ScopedKeyResolutionTests(unittest.TestCase):
    def test_scoped_miss_ignores_process_and_file_fallback(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            env_file = Path(temp_dir) / "legacy.env"
            env_file.write_text(plugin._KEY_NAME + "=file-secret\n")
            with _secret_scope_module(lambda _name: None), patch.dict(os.environ, {
                plugin._KEY_NAME: "process-secret", "CONTEXTLATTICE_ENV_FILE": "",
            }):
                self.assertEqual(plugin._read_key(str(env_file)), "")

    def test_scoped_value_overrides_legacy_sources(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            env_file = Path(temp_dir) / "legacy.env"
            env_file.write_text(plugin._KEY_NAME + "=file-secret\n")
            with _secret_scope_module(lambda _name: "scoped-secret"), patch.dict(os.environ, {
                plugin._KEY_NAME: "process-secret", "CONTEXTLATTICE_ENV_FILE": "",
            }):
                self.assertEqual(plugin._read_key(str(env_file)), "scoped-secret")

    def test_legacy_fallback_only_without_scoped_resolver(self):
        with tempfile.TemporaryDirectory() as temp_dir:
            env_file = Path(temp_dir) / "legacy.env"
            with _secret_scope_module(), patch.dict(os.environ, {
                plugin._KEY_NAME: "", "CONTEXTLATTICE_ENV_FILE": "",
            }):
                self.assertEqual(plugin._read_key(str(env_file)), "")
                os.environ[plugin._KEY_NAME] = "legacy-process-secret"
                self.assertEqual(plugin._read_key(str(env_file)), "legacy-process-secret")
                os.environ[plugin._KEY_NAME] = ""
                env_file.write_text(plugin._KEY_NAME + "=legacy-file-secret\n")
                self.assertEqual(plugin._read_key(str(env_file)), "legacy-file-secret")

    def test_resolver_exception_fails_closed(self):
        def fail(_name):
            raise RuntimeError("bound scope unavailable")

        with tempfile.TemporaryDirectory() as temp_dir:
            env_file = Path(temp_dir) / "legacy.env"
            env_file.write_text(plugin._KEY_NAME + "=file-secret\n")
            with _secret_scope_module(fail), patch.dict(os.environ, {
                plugin._KEY_NAME: "process-secret", "CONTEXTLATTICE_ENV_FILE": "",
            }):
                self.assertEqual(plugin._read_key(str(env_file)), "")


class FakeCL:
    """Records requests; behaviour is set per test via attributes."""

    def __init__(self):
        self.calls = []
        self.files = set()
        self.fail_paths = set()
        self.search_response = {"results": []}
        self.pack_response = {"reference_prompt": "PACKED",
                              "context_pack": {"project": "hermes-agent"},
                              "context_pack_quality": {"sample_id": "cpq_1"}}
        self.pack_sample_ids = []
        self.poll_responses = []
        self.omit_write_digest = False
        self.omit_write_lifecycle = False
        self.omit_write_ok = False
        self.write_records = []
        self.session_override = None
        fake = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _send(self, code, obj):
                body = json.dumps(obj).encode()
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body)

            def do_POST(self):
                n = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(n) or b"{}")
                fake.calls.append(("POST", self.path, body))
                if self.path in fake.fail_paths:
                    return self._send(500, {"error": "fake-secret-marker"})
                if self.path == "/v1/agents/sessions/start":
                    row = dict(body, id="sess_" + hashlib.sha256(body["native_session_id"].encode()).hexdigest()[:24])
                    return self._send(200, {"ok": True, "session": fake.session_override or row})
                if self.path == "/memory/search":
                    return self._send(200, fake.search_response)
                if self.path == "/memory/context-pack":
                    response = dict(fake.pack_response)
                    if fake.pack_sample_ids:
                        quality = dict(response.get("context_pack_quality") or {})
                        quality["sample_id"] = fake.pack_sample_ids.pop(0)
                        response["context_pack_quality"] = quality
                    return self._send(200, response)
                if self.path == "/memory/write":
                    fake.files.add(body["fileName"])
                    fake.write_records.append(body)
                    response = {"ok": True, "lifecycle": body.get("lifecycle", "durable"),
                                "project": body["projectName"], "file": body["fileName"]}
                    if fake.omit_write_ok:
                        response.pop("ok")
                    if fake.omit_write_lifecycle:
                        response.pop("lifecycle")
                    if not fake.omit_write_digest:
                        response["content_hash"] = "sha256:" + hashlib.sha256(
                            body["content"].encode()
                        ).hexdigest()
                    return self._send(200, response)
                return self._send(200, {"ok": True})

            def do_GET(self):
                fake.calls.append(("GET", self.path, None))
                if self.path.startswith("/memory/files/"):
                    file = self.path.split("/", 4)[4]
                    return self._send(200 if file in fake.files else 404, {})
                if self.path.startswith("/memory/search/continuations/"):
                    resp = fake.poll_responses.pop(0) if fake.poll_responses else {"status": "completed"}
                    return self._send(200, resp)
                return self._send(404, {})

        self.server = HTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def paths(self, method="POST"):
        return [p for m, p, _ in self.calls if m == method]

    def close(self):
        self.server.shutdown()
        self.server.server_close()


class Base(unittest.TestCase):
    def setUp(self):
        self.cl = FakeCL()
        self.temp_home = tempfile.TemporaryDirectory()
        self.home = Path(self.temp_home.name)
        (self.home / "memories").mkdir()
        self.p = plugin.ContextLatticeProvider()
        self.p._cfg = dict(plugin._DEFAULTS, url=self.cl.url, timeout=3.0, drain_seconds=2.0)
        self.p._key = "test-key"
        self.p._home = self.home
        self.p._session_id = "s1"

    def tearDown(self):
        self.p.shutdown()
        self.cl.close()
        self.temp_home.cleanup()


class PrefetchTests(Base):
    def test_outage_is_explicit_degraded(self):
        self.p._cfg["url"] = "http://127.0.0.1:9"  # nothing listens
        out = self.p.prefetch("anything")
        self.assertTrue(out.startswith("CONTEXTLATTICE DEGRADED"))

    def test_hits_with_continuation_are_not_called_warming(self):
        self.cl.search_response = {"results": [{"project": "p", "file": "f.md", "summary": "s"}],
                                   "continuation_async": {"pending_sources": ["letta"]}}
        out = self.p.prefetch("q")
        self.assertIn("fast-source hits are complete", out)
        self.assertNotIn("WARMING", out)

    def test_empty_with_continuation_is_warming_not_absent(self):
        self.cl.search_response = {"results": [], "continuation_async": {"pending_sources": ["letta"]}}
        out = self.p.prefetch("q")
        self.assertIn("WARMING, not absent", out)


class SearchDrainTests(Base):
    def test_cl_search_drains_continuation_and_merges(self):
        self.cl.search_response = {"results": [{"file": "a.md"}],
                                   "continuation_async": {"poll_url": "/memory/search/continuations/t1"}}
        self.cl.poll_responses = [
            {"status": "running"},
            {"status": "completed", "result": {"results": [{"file": "a.md"}, {"file": "b.md"}],
                                               "source_summary": {"failed_sources": ["letta"]}}},
        ]
        out = json.loads(self.p.handle_tool_call("cl_search", {"query": "q"}))
        self.assertEqual([r["file"] for r in out["results"]], ["a.md", "b.md"])
        self.assertEqual(out["continuation"]["status"], "completed")
        self.assertEqual(out["continuation"]["added_results"], 1)
        self.assertEqual(out["continuation"]["failed_sources"], ["letta"])

    def test_drain_false_skips_polling(self):
        self.cl.search_response = {"results": [], "continuation_async": {"poll_url": "/memory/search/continuations/t"}}
        out = json.loads(self.p.handle_tool_call("cl_search", {"query": "q", "drain": False}))
        self.assertEqual(out["continuation"]["status"], "not_drained")
        self.assertEqual(self.cl.paths("GET"), [])

    def test_write_readback_requires_nonempty_matching_digest_and_identity(self):
        project, file, content = "alpha", "notes/result.md", "written note"
        digest = "sha256:" + hashlib.sha256(content.encode()).hexdigest()
        args = {"project": project, "topic_path": "notes", "file": file, "content": content}

        self.cl.omit_write_digest = True
        self.cl.search_response = {"results": [{"project": project, "file": file}]}
        out = json.loads(self.p.handle_tool_call("cl_write", args))
        self.assertFalse(out["readback_found"])
        self.assertEqual(out["readback_status"], "unverified")
        self.assertIsNone(out["content_hash"])

        self.cl.omit_write_digest = False
        for row in (
            {"project": "other", "file": file, "content_hash": digest},
            {"project": project, "file": "notes/other.md", "content_hash": digest},
            {"file": file, "content_hash": digest},
            {"project": project, "content_hash": digest},
            {"project": project, "file": file, "content_hash": "sha256:wrong"},
            {"project": project, "file": file, "content_hash": "sha256:" + "g" * 64},
        ):
            self.cl.search_response = {"results": [row]}
            out = json.loads(self.p.handle_tool_call("cl_write", args))
            self.assertFalse(out["readback_found"], row)

        self.cl.search_response = {
            "results": [{"projectName": project, "fileName": file,
                         "content_hash": "SHA256:" + hashlib.sha256(content.encode()).hexdigest().upper()}]
        }
        out = json.loads(self.p.handle_tool_call("cl_write", args))
        self.assertTrue(out["readback_found"])
        self.assertEqual(out["readback_status"], "verified")

    def test_cl_write_without_explicit_ok_is_not_reported_written(self):
        self.cl.omit_write_ok = True
        self.cl.search_response = {"results": []}
        out = json.loads(self.p.handle_tool_call(
            "cl_write", {"project": "alpha", "topic_path": "notes", "file": "notes/x.md", "content": "x"}
        ))
        self.assertIsNot(out.get("written"), True)
        self.assertEqual(self.cl.paths("POST").count("/memory/search"), 0)

    def test_tool_error_receipt_redacts_response_body(self):
        self.cl.fail_paths.add("/memory/write")
        out = self.p.handle_tool_call(
            "cl_write",
            {"project": "alpha", "topic_path": "notes", "file": "notes/x.md", "content": "x"},
        )
        self.assertNotIn("fake-secret-marker", out)
        self.assertEqual(json.loads(out)["error"], "HTTPError")


class DelegationTests(Base):
    def test_batch_gets_context_pack_and_keeps_fields(self):
        args = {"tasks": [{"goal": "g1", "context": "orig", "toolsets": ["terminal"]}, {"goal": "g2"}]}
        d = self.p._on_pre_tool_call(tool_name="delegate_task", args=args)
        self.assertEqual(d["action"], "modify")
        t0, t1 = d["args"]["tasks"]
        self.assertTrue(t0["context"].startswith("orig"))
        self.assertIn(plugin._PACK_HEADER, t0["context"])
        self.assertIn("PACKED", t1["context"])
        self.assertEqual(t0["toolsets"], ["terminal"])
        packs = [b for m, p, b in self.cl.calls if p == "/memory/context-pack"]
        self.assertEqual(len(packs), 2)
        self.assertEqual(packs[0]["task_class"], "hermes.delegate_task")
        self.assertEqual(packs[0]["project"], self.p._cfg["project"])
        self.assertIn("target_context_pack_tokens", packs[0])

    def test_pack_failure_falls_back_to_search(self):
        self.cl.fail_paths.add("/memory/context-pack")
        self.cl.search_response = {"results": [{"project": "hermes-agent", "file": "x.md", "summary": "s"}]}
        d = self.p._on_pre_tool_call(tool_name="delegate_task", args={"goal": "g"})
        self.assertIn(plugin._SEARCH_HEADER, d["args"]["context"])

    def test_other_tools_and_subagents_untouched(self):
        self.assertIsNone(self.p._on_pre_tool_call(tool_name="terminal", args={"command": "ls"}))
        self.p._primary = False
        self.assertIsNone(self.p._on_pre_tool_call(tool_name="delegate_task", args={"goal": "g"}))
        self.assertEqual(self.cl.calls, [])

    def test_injected_block_is_capped(self):
        self.cl.pack_response = {"reference_prompt": "x" * 50000,
                                 "context_pack": {"project": "hermes-agent"}}
        self.p._cfg["delegation_max_chars"] = 500
        d = self.p._on_pre_tool_call(tool_name="delegate_task", args={"goal": "g"})
        self.assertLess(len(d["args"]["context"]), 500 + len(plugin._PACK_HEADER) + 5)

    def test_post_tool_call_reports_each_child_observation_not_success(self):
        args = {"tasks": [{"goal": "g1"}, {"goal": "g2"}]}
        self.cl.pack_response = {"reference_prompt": "P", "context_pack": {"project": "hermes-agent"},
                                 "context_pack_quality": {"sample_id": "cpq_x"}}
        self.p._on_pre_tool_call(tool_name="delegate_task", args=args)
        result = json.dumps({"results": [{"task_index": 0, "status": "completed"},
                                         {"task_index": 1, "status": "failed"}]})
        self.p._on_post_tool_call(tool_name="delegate_task", args=args, result=result)
        self.p.shutdown()
        outs = [b["metadata"] for m, p, b in self.cl.calls if p.endswith("/events")]
        self.assertEqual(sorted(o["host_reported_status"] for o in outs), ["completed", "failed"])
        self.assertTrue(all(o["host_status"] == "unknown" for o in outs))
        self.assertTrue(all(o["first_pass_success_known"] is False for o in outs))
        self.assertNotIn("/telemetry/context-pack-quality/outcome", self.cl.paths())
        self.assertEqual(self.p._pending_outcomes, {})

    def test_invocation_ids_do_not_collide_for_repeated_goals(self):
        args = {"goal": "repeat this exact task"}
        self.p._on_pre_tool_call(tool_name="delegate_task", args=args)
        self.p._on_pre_tool_call(tool_name="delegate_task", args=args)
        requests = [body for method, path, body in self.cl.calls if path == "/memory/context-pack"]
        self.assertEqual(len(requests), 2)
        self.assertNotEqual(requests[0]["task_id"], requests[1]["task_id"])
        key = self.p._delegation_key(["repeat this exact task"], "s1", "")
        self.assertEqual(len(self.p._pending_outcomes[key]), 2)
        self.assertIn(key, self.p._ambiguous_outcomes)

        result = json.dumps({"results": [{"task_index": 0, "status": "completed"}]})
        self.p._on_post_tool_call(tool_name="delegate_task", args=args, result=result)
        self.p._on_post_tool_call(tool_name="delegate_task", args=args, result=result)
        self.p.shutdown()
        outcomes = [body for method, path, body in self.cl.calls if path.endswith("/events")]
        self.assertEqual(outcomes, [])
        self.assertEqual(self.p._pending_outcomes, {})
        self.assertEqual(self.p._pending_invocations, {})
        self.assertEqual(self.p._ambiguous_outcomes, set())

    def test_ambiguous_identical_delegations_abstain_on_reverse_completion(self):
        args = {"goal": "same task under concurrent completion"}
        self.cl.pack_sample_ids = ["sample_first", "sample_second"]
        with self.assertLogs(plugin.logger, level="WARNING") as logs:
            self.p._on_pre_tool_call(tool_name="delegate_task", args=args)
            self.p._on_pre_tool_call(tool_name="delegate_task", args=args)
        self.assertEqual(len(logs.output), 1)
        key = self.p._delegation_key([args["goal"]], "s1", "")
        self.assertEqual([[r["sample_id"] for r in rows] for rows in self.p._pending_outcomes[key]],
                         [["sample_first"], ["sample_second"]])

        # The second invocation completes before the first, reversing FIFO association.
        for status in ("failed", "completed"):
            result = json.dumps({"results": [{"task_index": 0, "status": status}]})
            self.p._on_post_tool_call(tool_name="delegate_task", args=args, result=result)
        self.p.shutdown()
        outcomes = [body for method, path, body in self.cl.calls if path.endswith("/events")]
        self.assertEqual(outcomes, [])
        self.assertEqual(self.p._pending_outcomes, {})
        self.assertEqual(self.p._pending_invocations, {})
        self.assertEqual(self.p._ambiguous_outcomes, set())

    def test_context_pack_scope_mismatch_is_rejected_before_scoped_fallback(self):
        self.cl.pack_response = {
            "reference_prompt": "FOREIGN PACK",
            "context_pack": {"project": "other-project"},
            "context_pack_quality": {"sample_id": "cpq_foreign"},
        }
        self.cl.search_response = {
            "results": [{"project": "hermes-agent", "file": "scoped.md", "summary": "SCOPED FALLBACK"}]
        }
        result = self.p._on_pre_tool_call(tool_name="delegate_task", args={"goal": "g"})
        context = result["args"]["context"]
        self.assertIn("SCOPED FALLBACK", context)
        self.assertNotIn("FOREIGN PACK", context)

    def test_fallback_rejects_unscoped_results(self):
        self.cl.fail_paths.add("/memory/context-pack")
        self.cl.search_response = {
            "results": [{"project": "other-project", "file": "foreign.md", "summary": "FOREIGN DATA"}]
        }
        result = self.p._on_pre_tool_call(tool_name="delegate_task", args={"goal": "g"})
        context = result["args"]["context"]
        self.assertIn("BLOCKED", context)
        self.assertNotIn("FOREIGN DATA", context)


class DeliveryObservationTests(Base):
    def inject(self, call="call-a", session="s1", goal="g", **fields):
        args = {"goal": goal, **fields}
        directive = self.p._on_pre_tool_call(tool_name="delegate_task", args=args,
                                            session_id=session, tool_call_id=call)
        return dict(args, **(directive or {}).get("args", {}))

    def finish(self, args, call="call-a", session="s1", entries=None):
        if entries is None:
            entries = [{"task_index": 0, "status": "completed"}]
        self.p._on_post_tool_call(tool_name="delegate_task", args=args,
                                 session_id=session, tool_call_id=call, result={"results": entries})
        self.p.shutdown()
        return [b["metadata"] for m, path, b in self.cl.calls if path.endswith("/events")]

    def test_reverse_same_goals_bind_host_ids_not_fifo(self):
        self.cl.pack_sample_ids = ["sample_a", "sample_b"]
        a = self.inject(call="a")
        b = self.inject(call="b")
        self.finish(b, call="b", entries=[{"task_index": 0, "status": "failed"}])
        rows = self.finish(a, call="a")
        self.assertEqual([(r["sample_id"], r["host_reported_status"]) for r in rows],
                         [("sample_b", "failed"), ("sample_a", "completed")])
        self.assertTrue(all(r["post_hook_context_match"] for r in rows))
        self.assertEqual(len({r["task_id"] for r in rows}), 2)

    def test_exact_rendered_bytes_and_project_session_binding(self):
        args = self.inject(context="existing\ntext")
        row = self.finish(args)[0]
        pack = next(b for _, p, b in self.cl.calls if p == "/memory/context-pack")
        self.assertEqual(row["session_id"], pack["session_id"])
        self.assertEqual(row["task_id"], pack["task_id"])
        self.assertEqual(row["context_digest"], plugin._text_digest(args["context"]))
        self.assertEqual(row["context_bytes"], len(args["context"].encode()))
        self.assertNotIn("existing", json.dumps(row))
        self.assertEqual(pack["agent_id"], "hermes_agent")
        self.assertFalse(row["independent_verification"])
        self.assertFalse(row["consumption_observed"])
        self.assertFalse(row["dispatch_observed"])
        self.assertFalse(row["delivery_confirmed"])

    def test_downstream_context_change_is_not_delivery_confirmation(self):
        args = self.inject()
        args["context"] += " changed by later middleware"
        self.assertFalse(self.finish(args)[0]["post_hook_context_match"])

    def test_session_switch_does_not_rebind_inflight_call(self):
        args = self.inject(session="old")
        self.p.on_session_switch("new")
        self.assertEqual(self.finish(args, session="new"), [])
        row = self.finish(args, session="old")[0]
        self.assertEqual(row["native_session_digest"], plugin._text_digest("old"))

    def test_session_mismatch_keeps_recall_without_observation_authority(self):
        self.cl.session_override = {"id": "sess_foreign", "project": "wrong",
                                    "agent_id": "hermes_agent", "native_session_id": "s1"}
        args = self.inject()
        self.assertIn("PACKED", args["context"])
        self.assertEqual(self.finish(args), [])
        pack = next(b for _, p, b in self.cl.calls if p == "/memory/context-pack")
        self.assertNotIn("session_id", pack)

    def test_duplicate_and_boolean_indices_never_assign_success(self):
        cases = [[{"task_index": True, "status": "completed"}],
                 [{"task_index": 0, "status": "failed"}, {"task_index": 0, "status": "completed"}],
                 [{"task_index": 4, "status": "completed"}],
                 [{"task_index": -1, "status": "completed"}]]
        for n, entries in enumerate(cases):
            with self.subTest(n=n):
                args = self.inject(call=f"bad-{n}")
                row = self.finish(args, call=f"bad-{n}", entries=entries)[-1]
                self.assertEqual(row["host_status"], "unknown")
                self.assertFalse(row["first_pass_success_known"])

    def test_completed_with_schema_failure_is_observation_only(self):
        args = self.inject()
        row = self.finish(args, entries=[{"task_index": 0, "status": "completed",
                                         "schema_valid": False, "truncated": True}])[0]
        self.assertFalse(row["schema_valid"])
        self.assertTrue(row["truncated"])
        self.assertFalse(row["retry_count_known"])
        self.assertFalse(row["calibration_eligible"])
        self.assertNotIn("/telemetry/context-pack-quality/outcome", self.cl.paths())

    def test_duplicate_post_and_pre_are_not_double_counted(self):
        args = self.inject()
        self.finish(args)
        self.assertEqual(len(self.finish(args)), 1)
        self.assertIsNone(self.p._on_pre_tool_call(tool_name="delegate_task", args={"goal": "g"},
                                                  session_id="s1", tool_call_id="call-a"))
        self.assertEqual(sum(p == "/memory/context-pack" for _, p, _ in self.cl.calls), 1)

    def test_changed_goal_and_missing_results_never_invent_success(self):
        args = self.inject()
        self.assertEqual(self.finish(dict(args, goal="different")), [])
        args = self.inject(call="missing")
        self.assertEqual(self.finish(args, call="missing", entries=[])[0]["host_status"], "unknown")

    def test_bounds_and_expiry_remain_explicit(self):
        tasks = [{"goal": "g"}] * (plugin._DELEGATION_MAX_TASKS + 1)
        self.assertIsNone(self.p._on_pre_tool_call(tool_name="delegate_task", args={"tasks": tasks}))
        self.assertEqual(self.cl.calls, [])
        args = self.inject()
        with patch.object(plugin.time, "monotonic", return_value=plugin.time.monotonic() + 3601):
            self.assertEqual(self.finish(args), [])
        self.assertEqual(self.p._pending_outcomes, {})
        self.assertEqual(self.p._pending_invocations, {})

    def test_goal_separator_collision_is_not_same_invocation(self):
        self.assertNotEqual(plugin._goals_key(["a\x1fb", "c"]), plugin._goals_key(["a", "b\x1fc"]))

    def test_session_ensure_is_cached_and_no_new_prompt_instruction(self):
        self.finish(self.inject())
        self.finish(self.inject(call="second"), call="second")
        self.assertEqual(sum(p.endswith("/start") for _, p, _ in self.cl.calls), 1)
        self.assertNotIn("verified utility", self.p.system_prompt_block())

    def test_cancelled_callback_generation_cannot_attach_to_successor(self):
        original = {"goal": "repeat"}
        entered, release = threading.Event(), threading.Event()
        old_pack = self.p._delegation_pack

        def stalled(goal, task, session=""):
            entered.set()
            self.assertTrue(release.wait(3))
            return "late context", "late_sample"

        with patch.object(self.p, "_delegation_pack", side_effect=stalled):
            first = threading.Thread(target=lambda: self.p._on_pre_tool_call(tool_name="delegate_task", args=original))
            first.start()
            self.assertTrue(entered.wait(3))
            self.p._on_post_tool_call(tool_name="delegate_task", args=original, result={})
            with patch.object(self.p, "_delegation_pack", side_effect=old_pack):
                directive = self.p._on_pre_tool_call(tool_name="delegate_task", args=original)
            release.set()
            first.join(3)
            self.assertFalse(first.is_alive())
        self.p._on_post_tool_call(tool_name="delegate_task", args=dict(original, **directive["args"]),
                                 result={"results": [{"task_index": 0, "status": "completed"}]})
        self.p.shutdown()
        rows = [b["metadata"] for _, path, b in self.cl.calls if path.endswith("/events")]
        self.assertEqual([row["sample_id"] for row in rows], ["cpq_1"])
        self.assertEqual(self.p._pending_outcomes, {})
        self.assertEqual(self.p._pending_generations, {})

    def test_event_failure_invalidates_only_its_binding(self):
        args = self.inject()
        self.cl.fail_paths.add("/v1/agents/sessions/sess_" + plugin._text_digest("s1")[:24] + "/events")
        self.finish(args)
        self.assertEqual(self.p._session_bindings, {})
        self.cl.fail_paths.clear()
        self.finish(self.inject(call="new"), call="new")
        self.assertEqual(sum(path.endswith("/start") for _, path, _ in self.cl.calls), 2)

    def test_cached_binding_is_revalidated_after_bounded_interval(self):
        self.finish(self.inject())
        with patch.object(plugin.time, "monotonic", return_value=plugin.time.monotonic() + 61):
            self.finish(self.inject(call="next"), call="next")
        self.assertEqual(sum(path.endswith("/start") for _, path, _ in self.cl.calls), 2)


class MirrorReconcileTests(Base):
    def test_failed_mirror_goes_to_outbox_then_drains(self):
        self.cl.fail_paths.add("/memory/write")
        self.p.on_memory_write("add", "memory", "rule one")
        self.p.shutdown()
        self.assertTrue(self.p._outbox_path().exists())
        self.cl.fail_paths.clear()
        self.assertEqual(self.p.drain_outbox(), {"sent": 1, "kept": 0, "blocked": 0})
        self.assertFalse(self.p._outbox_path().exists())
        self.assertIn(plugin._mirror_target("rule one")[1], self.cl.files)

    def test_replace_and_remove_retire_the_previous_identity(self):
        old_file = plugin._mirror_target("old rule")[1]
        new_file = plugin._mirror_target("new rule")[1]
        self.p.on_memory_write("replace", "memory", "new rule",
                               metadata={"previous_content": "old rule"})
        self.p.shutdown()
        replace_writes = {row["fileName"]: row for row in self.cl.write_records}
        self.assertEqual(replace_writes[old_file]["lifecycle"], "superseded")
        self.assertTrue(replace_writes[old_file]["content"].startswith("SUPERSEDED"))
        self.assertEqual(replace_writes[new_file]["lifecycle"], "durable")

        self.cl.write_records.clear()
        self.p.on_memory_write("remove", "memory", "",
                               metadata={"previous_content": "new rule"})
        self.p.shutdown()
        remove = self.cl.write_records[-1]
        self.assertEqual(remove["fileName"], new_file)
        self.assertEqual(remove["lifecycle"], "retired")
        self.assertTrue(remove["content"].startswith("RETIRED"))

    def test_unconfirmed_lifecycle_is_retained_and_reported_blocked(self):
        self.cl.omit_write_lifecycle = True
        self.p.on_memory_write("remove", "memory", "old rule")
        self.p.shutdown()
        drained = self.p.drain_outbox()
        self.assertEqual(drained, {"sent": 0, "kept": 1, "blocked": 1})
        self.assertTrue(self.p._outbox_path().exists())

    def test_outbox_appends_are_atomic_across_provider_instances(self):
        providers = [plugin.ContextLatticeProvider() for _ in range(6)]
        for provider in providers:
            provider._home = self.home
        errors = []

        def append_many(provider, worker):
            try:
                for index in range(12):
                    provider._outbox_append({
                        "project": "hermes-agent", "topic_path": "memory",
                        "file": f"notes/{worker}-{index}.md", "content": f"item {worker}-{index}",
                    })
            except Exception as exc:
                errors.append(type(exc).__name__)

        threads = [threading.Thread(target=append_many, args=(provider, worker))
                   for worker, provider in enumerate(providers)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertEqual(errors, [])
        rows = plugin.ContextLatticeProvider._read_outbox(self.p._outbox_path())
        self.assertEqual(len(rows), 72)
        self.assertEqual(len({row["_outbox_id"] for row in rows}), 72)

    def test_concurrent_outbox_drains_across_instances_do_not_lose_items(self):
        other = plugin.ContextLatticeProvider()
        other._cfg = dict(self.p._cfg)
        other._key = self.p._key
        other._home = self.home
        for index in range(8):
            self.p._outbox_append({
                "project": "hermes-agent", "topic_path": "memory",
                "file": f"notes/drain-{index}.md", "content": f"item {index}",
            })
        results = []
        threads = [threading.Thread(target=lambda provider=provider: results.append(provider.drain_outbox()))
                   for provider in (self.p, other)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        other.shutdown()
        self.assertEqual(sum(result["sent"] for result in results), 8)
        self.assertEqual(sum(result["kept"] for result in results), 0)
        self.assertFalse(self.p._outbox_path().exists())

    def test_outbox_limit_fails_without_dropping_existing_items(self):
        original_limit = plugin._OUTBOX_MAX_ITEMS
        try:
            plugin._OUTBOX_MAX_ITEMS = 1
            self.p._outbox_append({"project": "hermes-agent", "topic_path": "memory",
                                   "file": "notes/one.md", "content": "one"})
            with self.assertRaises(RuntimeError):
                self.p._outbox_append({"project": "hermes-agent", "topic_path": "memory",
                                       "file": "notes/two.md", "content": "two"})
        finally:
            plugin._OUTBOX_MAX_ITEMS = original_limit
        self.assertEqual(len(plugin.ContextLatticeProvider._read_outbox(self.p._outbox_path())), 1)

    def test_reconcile_backfills_only_missing_entries(self):
        (self.home / "memories" / "MEMORY.md").write_text("alpha rule\n§\nbeta rule")
        (self.home / "memories" / "USER.md").write_text("user pref")
        self.cl.files.add(plugin._mirror_target("alpha rule")[1])
        r = self.p.reconcile(repair=True)
        self.assertEqual((r["checked"], r["present"], r["repaired"]), (3, 1, 2))
        again = self.p.reconcile(repair=True)
        self.assertEqual((again["present"], again["repaired"]), (3, 0))

    def test_reconcile_dry_run_writes_nothing(self):
        (self.home / "memories" / "MEMORY.md").write_text("gamma rule")
        r = self.p.reconcile(repair=False)
        self.assertEqual((r["missing"] != [], r["repaired"]), (True, 0))
        self.assertNotIn("/memory/write", self.cl.paths())
        self.assertFalse((self.home / "contextlattice-plugin" / "reconcile.json").exists())

    def test_reconcile_command_dry_run_does_not_drain_outbox(self):
        item = {"project": "hermes-agent", "topic_path": "memory/durable/rule",
                "file": "notes/rule.md", "content": "rule"}
        self.p._outbox_append(item)
        outbox = self.p._outbox_path()
        before = outbox.read_bytes()
        (self.home / "memories" / "MEMORY.md").write_text("gamma rule")

        result = self.p._cmd_reconcile("--dry-run")

        self.assertIn("outbox unchanged", result)
        self.assertEqual(outbox.read_bytes(), before)
        self.assertNotIn("/memory/write", self.cl.paths())
        self.assertFalse((self.home / "contextlattice-plugin" / "reconcile.json").exists())

    def test_corrupt_outbox_blocks_repair_without_remote_mutation(self):
        outbox = self.p._outbox_path()
        outbox.write_text("{invalid json\n")
        (self.home / "memories" / "MEMORY.md").write_text("gamma rule")

        result = self.p._cmd_reconcile("")

        self.assertIn("outbox BLOCKED", result)
        self.assertIn("reconciliation was not attempted", result)
        self.assertNotIn("/memory/write", self.cl.paths())
        self.assertFalse((self.home / "contextlattice-plugin" / "reconcile.json").exists())

    def test_pending_handoff_survives_warming_and_error_then_clears_on_complete_empty(self):
        self.p._pending_handoff = True
        self.cl.search_response = {
            "results": [], "continuation_async": {"pending_sources": ["memory_bank"]}
        }
        self.p.prefetch("query")
        self.assertTrue(self.p._pending_handoff)

        self.cl.search_response = {"error": "upstream failure", "results": []}
        self.p.prefetch("query")
        self.assertTrue(self.p._pending_handoff)

        self.cl.search_response = {"results": []}
        self.p.prefetch("query")
        self.assertFalse(self.p._pending_handoff)

    def test_mirror_and_reconcile_share_one_target(self):
        self.p.on_memory_write("add", "memory", "delta rule")
        self.p.shutdown()
        (self.home / "memories" / "MEMORY.md").write_text("delta rule")
        self.assertEqual(self.p.reconcile(repair=False)["present"], 1)


if __name__ == "__main__":
    unittest.main()
