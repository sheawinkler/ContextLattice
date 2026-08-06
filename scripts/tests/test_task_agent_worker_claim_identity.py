#!/usr/bin/env python3
"""Regression coverage for external task worker claim identity."""

from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path
from unittest import mock

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPTS_DIR = REPO_ROOT / "scripts"
if str(SCRIPTS_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_DIR))


def load_task_worker():
    path = SCRIPTS_DIR / "task_agent_worker.py"
    spec = importlib.util.spec_from_file_location("task_agent_worker_claim_identity_under_test", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("failed to load task_agent_worker.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


class TaskAgentWorkerClaimIdentityTests(unittest.TestCase):
    def test_claim_sends_same_trimmed_worker_in_query_and_body(self) -> None:
        worker = load_task_worker()
        captured: dict[str, object] = {}

        def fake_post(orchestrator_url, path, payload, params=None, *, timeout=30.0):
            captured.update(
                orchestrator_url=orchestrator_url,
                path=path,
                payload=payload,
                params=params,
                timeout=timeout,
            )
            return {"task": None}

        with mock.patch.object(worker, "_post", side_effect=fake_post):
            result = worker._claim_next_task("http://127.0.0.1:8075", "  hermes-agent  ")

        self.assertEqual(result, {"task": None})
        self.assertEqual(captured["path"], "/agents/tasks/next")
        self.assertEqual(captured["payload"], {"worker": "hermes-agent"})
        self.assertEqual(captured["params"], {"worker": "hermes-agent"})


if __name__ == "__main__":
    unittest.main()
