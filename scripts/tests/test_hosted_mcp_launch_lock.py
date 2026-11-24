import contextlib
import importlib.util
import io
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("hosted_mcp_lock", ROOT / "scripts/launch_lock.py")
lock = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = lock
spec.loader.exec_module(lock)

# An allowed candidate in fixtures only. No test opens a network connection.
URL = "https://fixture.contextlattice.io/mcp"


def initialized():
    return {"jsonrpc": "2.0", "id": 1, "result": {
        "protocolVersion": lock.PROTOCOL, "serverInfo": {"name": "contextlattice", "version": "1"},
        "capabilities": {"tools": {"listChanged": False}},
    }}


class HostedMCPLockTests(unittest.TestCase):
    def setUp(self):
        self.connection = self.enterContext(patch.object(lock.http.client, "HTTPSConnection", autospec=True))
        self.conn = self.connection.return_value
        self.response = self.conn.getresponse.return_value.__enter__.return_value
        self.response.status = 200
        self.response.getheader.return_value = "application/json; charset=utf-8"
        self.response.read.return_value = json.dumps(initialized()).encode()

    def run_lock(self, mode):
        with patch.object(sys, "argv", ["launch_lock.py", "--mode", mode]), \
                patch.object(lock, "_run_submission_preflight", return_value=lock.GateCheck(True, "preflight", "")) as preflight, \
                contextlib.redirect_stdout(io.StringIO()) as out:
            return lock.main(), out.getvalue(), preflight.call_args.kwargs["online"]

    def test_shipped_sources_local_go_public_no_go_without_network(self):
        for name in ("contextlattice.server.json", "contextlattice.server.template.json"):
            obj = json.loads((ROOT / "registry" / name).read_text())
            self.assertEqual(obj["remotes"], [])
            self.assertIn("stdio", obj["description"])
            self.assertIn("pending", obj["description"])
        for mode, code in (("public", 1), ("local", 0)):
            actual, details, online = self.run_lock(mode)
            self.assertEqual(actual, code)
            self.assertIn("hosted MCP unpublished", details)
            self.assertFalse(online)
        self.connection.assert_not_called()

    def test_untrusted_or_malformed_manifest_never_probes(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(lock, "ROOT", Path(tmp)), \
                patch.object(lock, "TRUSTED_MCP_URLS", {URL}):
            registry = Path(tmp) / "registry"
            registry.mkdir()
            src = registry / "contextlattice.server.json"
            good = {"type": "streamable-http", "url": URL}
            obj = {"name": "io.github.sheawinkler/contextlattice", "remotes": [good]}
            src.write_text(json.dumps(obj))
            self.assertEqual(lock._registry_remote(), URL)
            for bad in (None, {}, [good, good], [{**good, "headers": []}], [{**good, "type": "sse"}],
                        [{**good, "url": "https://REPLACE_WITH_PUBLIC_MCP_HOST/mcp"}],
                        [{**good, "url": "https://untrusted.contextlattice.io/mcp"}]):
                src.write_text(json.dumps({**obj, "remotes": bad}))
                for mode in ("local", "public"):
                    code, detail, online = self.run_lock(mode)
                    self.assertEqual(code, 1)
                    self.assertIn("invalid or untrusted manifest", detail)
                    self.assertFalse(online)
            for raw in ("{", "{}", '{"remotes":[],"remotes":[]}'):
                src.write_text(raw)
                self.assertEqual(self.run_lock("public")[0], 1)
        self.connection.assert_not_called()

    def test_origin_policy_even_for_accidentally_allowlisted_input(self):
        for bad in ("http://fixture.contextlattice.io/mcp", "https://127.0.0.1/mcp",
                    "https://contextlattice.io.evil.invalid/mcp", "https://user@contextlattice.io/mcp",
                    URL + "?key=private", URL + "#login", URL.replace("/mcp", ":8443/mcp"),
                    "https://REPLACE_WITH_PUBLIC_MCP_HOST/mcp"):
            with self.subTest(bad=bad), patch.object(lock, "TRUSTED_MCP_URLS", {bad}):
                self.assertFalse(lock._probe_public_mcp(bad, 2).ok)
        self.connection.assert_not_called()

    def test_valid_init_sends_no_credentials_and_is_not_registry_proof(self):
        with patch.object(lock, "TRUSTED_MCP_URLS", {URL}), patch.object(lock, "_registry_remote", return_value=URL), \
                patch.dict(os.environ, {"CONTEXTLATTICE_ORCHESTRATOR_API_KEY": "fixture-secret",
                                        "HTTPS_PROXY": "http://unused.invalid/"}):
            code, detail, online = self.run_lock("public")
        self.assertTrue(online)
        self.assertEqual(code, 1)
        self.assertIn("verified anonymous initialize only", detail)
        self.assertIn("matching registry registration :: not verified", detail)
        self.connection.assert_called_once_with("fixture.contextlattice.io", timeout=10)
        args, kwargs = self.conn.request.call_args
        self.assertEqual(args[:2], ("POST", "/mcp"))
        self.assertEqual(json.loads(args[2])["params"]["protocolVersion"], lock.PROTOCOL)
        self.assertNotIn(b"fixture-secret", args[2])
        self.assertEqual(set(kwargs["headers"]), {"Content-Type", "Accept"})
        self.response.read.assert_called_once_with(lock.MAX_BYTES + 1)
        self.conn.close.assert_called_once()

    def test_redirect_auth_and_errors_are_not_evidence(self):
        with patch.object(lock, "TRUSTED_MCP_URLS", {URL}):
            for status in (202, 301, 302, 303, 307, 308, 401, 403, 404, 405, 429, 500):
                with self.subTest(status=status):
                    self.response.status = status
                    self.assertFalse(lock._probe_public_mcp(URL, 2).ok)
            self.response.read.assert_not_called()
            self.assertEqual(self.conn.request.call_count, 12)
            self.conn.getresponse.side_effect = TimeoutError("private response")
            check = lock._probe_public_mcp(URL, 2)
            self.assertFalse(check.ok)
            self.assertNotIn("private response", check.details)

    def test_bad_200_responses_are_not_protocol_evidence(self):
        good = initialized()
        result = good["result"]
        bad = [b"<html>login</html>", b"{}", b"\xff", b" " * (lock.MAX_BYTES + 1),
               b'{"jsonrpc":"2.0","id":1,"result":{},"result":{}}']
        for change in ({"id": True}, {"id": 2}, {"jsonrpc": "1.0"}, {"error": {}},
                       {"result": None}, {"result": {**result, "protocolVersion": "other"}},
                       {"result": {**result, "serverInfo": {"name": "other", "version": "1"}}},
                       {"result": {**result, "serverInfo": {"name": "contextlattice"}}},
                       {"result": {**result, "capabilities": {}}},
                       {"result": {**result, "capabilities": {"tools": True}}},
                       {"result": {**result, "capabilities": {"tools": {"listChanged": 1}}}}):
            bad.append(json.dumps({**good, **change}).encode())
        with patch.object(lock, "TRUSTED_MCP_URLS", {URL}):
            for body in bad:
                self.response.read.return_value = body
                self.assertFalse(lock._probe_public_mcp(URL, 2).ok)
            self.response.read.return_value = json.dumps(good).encode()
            self.response.getheader.return_value = "text/html"
            self.assertFalse(lock._probe_public_mcp(URL, 2).ok)
