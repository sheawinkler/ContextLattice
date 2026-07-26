from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
GATE = ROOT / "scripts/agent/runtime-soak-gate"


class SoakHandler(BaseHTTPRequestHandler):
    ready = True
    continuation_pending = 0
    continuation_oldest_age_secs = 0.0
    sync_pending = 0
    sync_oldest_age_secs = 0.0

    def do_GET(self) -> None:
        if self.path == "/healthz":
            payload = {
                "ok": True,
                "ready": self.ready,
                "liveness": "healthy",
                "strictNoPythonRuntime": True,
                "backendRequired": False,
                "backendHealth": True,
                "backendStatus": "not_required",
                "memoryStore": {
                    "configured": True,
                    "ready": self.ready,
                    "phase": "ready" if self.ready else "hydrating",
                },
                "qdrantPayloadIndexes": {
                    "enabled": True,
                    "ready": self.ready,
                    "status": "ready" if self.ready else "waiting_for_memory_store",
                },
                "build": {
                    "source_commit": "commit-proof",
                    "source_tree": "tree-proof",
                },
            }
            self.send_json(200, payload)
            return
        if self.path == "/readyz":
            self.send_json(
                200 if self.ready else 503,
                {"ok": self.ready, "ready": self.ready},
            )
            return
        if self.path == "/status":
            self.send_json(
                200,
                {
                    "ok": True,
                    "queue": {
                        "pendingTotal": self.continuation_pending,
                        "oldestAgeSecs": self.continuation_oldest_age_secs,
                        "durableOldestAgeSecs": 0,
                        "syncLane": {
                            "pending_count": self.sync_pending,
                            "oldest_age_secs": self.sync_oldest_age_secs,
                        },
                    },
                },
            )
            return
        if self.path == "/ops/native-ownership":
            self.send_json(
                200,
                {
                    "ok": True,
                    "status": "clean",
                    "requiredRouteCount": 137,
                    "violationCount": 0,
                    "pythonHotPathOwnership": {"fallbacks": 0},
                },
            )
            return
        self.send_json(404, {"error": "not found"})

    def send_json(self, status: int, payload: dict[str, object]) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format: str, *_args: object) -> None:
        return


class RuntimeSoakGateTests(unittest.TestCase):
    def setUp(self) -> None:
        SoakHandler.ready = True
        SoakHandler.continuation_pending = 0
        SoakHandler.continuation_oldest_age_secs = 0.0
        SoakHandler.sync_pending = 0
        SoakHandler.sync_oldest_age_secs = 0.0
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), SoakHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base_url = f"http://127.0.0.1:{self.server.server_port}"

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def run_gate(self, *extra: str) -> tuple[subprocess.CompletedProcess[str], dict[str, object]]:
        result = subprocess.run(
            [
                sys.executable,
                str(GATE),
                "--base-url",
                self.base_url,
                "--duration-secs",
                "0",
                "--request-timeout-secs",
                "1",
                "--expect-source-commit",
                "commit-proof",
                "--expect-source-tree",
                "tree-proof",
                *extra,
            ],
            cwd=ROOT,
            capture_output=True,
            check=False,
            text=True,
            timeout=10,
        )
        return result, json.loads(result.stdout)

    def test_ready_runtime_passes_and_writes_owner_only_artifact(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            output = Path(temp_dir) / "soak.json"
            result, payload = self.run_gate("--output", str(output))
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue(payload["ok"])
            self.assertEqual(payload["sample_count"], 1)
            self.assertEqual(payload["failure_count"], 0)
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)
            self.assertEqual(json.loads(output.read_text(encoding="utf-8"))["schema_id"], payload["schema_id"])

    def test_warming_runtime_fails_closed(self) -> None:
        SoakHandler.ready = False
        result, payload = self.run_gate()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(payload["ok"])
        checks = {failure["check"] for failure in payload["failures"]}
        self.assertIn("readyz_http_status", checks)
        self.assertIn("runtime_readiness", checks)
        self.assertIn("memory_store_ready", checks)

    def test_source_identity_mismatch_fails(self) -> None:
        result, payload = self.run_gate("--expect-source-tree", "different-tree")
        self.assertNotEqual(result.returncode, 0)
        checks = {failure["check"] for failure in payload["failures"]}
        self.assertIn("source_tree", checks)

    def test_bounded_continuation_does_not_use_sync_queue_age_limit(self) -> None:
        SoakHandler.continuation_pending = 1
        SoakHandler.continuation_oldest_age_secs = 60.0
        result, payload = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(payload["ok"])
        queue = payload["samples"][0]["queue"]
        self.assertEqual(queue["continuation_oldest_age_secs"], 60.0)
        self.assertEqual(queue["sync_oldest_age_secs"], 0.0)

    def test_stale_sync_queue_still_fails_at_runtime_high_threshold(self) -> None:
        SoakHandler.sync_pending = 1
        SoakHandler.sync_oldest_age_secs = 6.0
        result, payload = self.run_gate()
        self.assertNotEqual(result.returncode, 0)
        checks = {failure["check"] for failure in payload["failures"]}
        self.assertIn("sync_queue_oldest_age_secs", checks)


if __name__ == "__main__":
    unittest.main()
