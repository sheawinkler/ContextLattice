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


def test_gateway_inference_enabled_default_and_override(monkeypatch):
    monkeypatch.delenv("TASK_INFERENCE_GATEWAY_ENABLED", raising=False)
    assert taw._gateway_inference_enabled() is True
    monkeypatch.setenv("TASK_INFERENCE_GATEWAY_ENABLED", "false")
    assert taw._gateway_inference_enabled() is False


def test_run_llm_task_via_gateway_posts_inference_chat(monkeypatch):
    captured: dict[str, object] = {}

    def fake_post(orchestrator_url, path, payload, params=None, *, timeout=30.0):
        captured["orchestrator_url"] = orchestrator_url
        captured["path"] = path
        captured["payload"] = payload
        captured["timeout"] = timeout
        return {
            "ok": True,
            "content": "gateway reply",
            "route": {
                "provider": "ollama",
                "base_url": "http://127.0.0.1:11434",
                "reason": "explicit ollama provider",
            },
        }

    monkeypatch.setattr(taw, "_post", fake_post)
    output, route = taw._run_llm_task_via_gateway(
        "http://127.0.0.1:8075",
        "auto",
        "qwen3.5:9b",
        {"title": "Run task", "payload": {"alpha": 1}},
        context_prompt="facts only",
    )
    assert output == "gateway reply"
    assert route.get("provider") == "ollama"
    assert captured["orchestrator_url"] == "http://127.0.0.1:8075"
    assert captured["path"] == "/v1/inference/chat"
    assert captured["timeout"] == 95.0
    payload = captured["payload"]
    assert isinstance(payload, dict)
    assert payload.get("provider") == "auto"
    assert payload.get("model") == "qwen3.5:9b"
