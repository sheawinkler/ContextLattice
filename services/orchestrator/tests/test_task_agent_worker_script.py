from __future__ import annotations

from pathlib import Path
import sys


REPO_ROOT = Path(__file__).resolve().parents[3]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from scripts import task_agent_worker as taw


def test_runner_cmd_for_hermes_agent_prefers_hermes_agent_cmd(monkeypatch):
    monkeypatch.delenv("TASK_AGENT_CMD", raising=False)
    monkeypatch.setenv("HERMES_AGENT_CMD", "hermes-agent run")
    monkeypatch.setenv("HERMES_CMD", "hermes run")

    assert taw._runner_cmd_for_agent("hermes-agent") == "hermes-agent run"
    assert taw._runner_cmd_for_agent("hermes") == "hermes-agent run"


def test_runner_cmd_for_hermes_falls_back_to_hermes_cmd(monkeypatch):
    monkeypatch.delenv("TASK_AGENT_CMD", raising=False)
    monkeypatch.delenv("HERMES_AGENT_CMD", raising=False)
    monkeypatch.setenv("HERMES_CMD", "hermes run")

    assert taw._runner_cmd_for_agent("hermes-agent") == "hermes run"
