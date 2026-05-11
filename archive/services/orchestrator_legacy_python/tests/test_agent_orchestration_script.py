from __future__ import annotations

from pathlib import Path
import sys


REPO_ROOT = Path(__file__).resolve().parents[3]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from scripts import agent_orchestration as ao


class _DummyResponse:
    def __init__(self, payload: dict):
        self._payload = payload
        self.status_code = 200

    def raise_for_status(self) -> None:
        return

    def json(self) -> dict:
        return self._payload


class _DummyClient:
    def __init__(self):
        self.calls: list[tuple[str, str, dict | None, dict | None]] = []

    def post(self, url: str, json: dict):
        self.calls.append(("post", url, json, None))
        if url.endswith("/memory/search"):
            return _DummyResponse(
                {
                    "results": [],
                    "warnings": [],
                    "retrieval_lifecycle": {
                        "status": "succeeded",
                        "result_state": "ready",
                        "sources": {"returned_now": ["qdrant"]},
                    },
                }
            )
        if url.endswith("/memory/context-pack"):
            return _DummyResponse({"context_pack": {"facts": [], "results": []}, "warnings": []})
        return _DummyResponse({})

    def get(self, url: str, params: dict | None = None):
        self.calls.append(("get", url, None, params))
        if url.endswith("/health"):
            return _DummyResponse({"ok": True})
        if url.endswith("/status"):
            return _DummyResponse({"service": {"ok": True}})
        return _DummyResponse({})

    def close(self) -> None:
        return


class _DummyContextLatticeClient:
    def __init__(self, dummy_client: _DummyClient):
        self.client = dummy_client

    def close(self) -> None:
        return


def test_search_with_lifecycle_sends_agent_id_and_topic(monkeypatch):
    dummy = _DummyClient()
    monkeypatch.setattr(ao, "ContextLatticeClient", lambda *args, **kwargs: _DummyContextLatticeClient(dummy))
    orch = ao.ContextLatticeOrchestrator("http://127.0.0.1:8075", agent_id="codex_gpt5_test")
    orch.search_with_lifecycle(
        query="latency baseline",
        project="contextlattice",
        topic_path="runbooks/perf",
        include_retrieval_debug=True,
        wait_for_completion=False,
    )
    post_calls = [c for c in dummy.calls if c[0] == "post" and c[1].endswith("/memory/search")]
    assert post_calls, "expected /memory/search call"
    payload = post_calls[-1][2] or {}
    assert payload.get("agent_id") == "codex_gpt5_test"
    assert payload.get("topic_path") == "runbooks/perf"


def test_context_pack_uses_stable_agent_id(monkeypatch):
    dummy = _DummyClient()
    monkeypatch.setattr(ao, "ContextLatticeClient", lambda *args, **kwargs: _DummyContextLatticeClient(dummy))
    orch = ao.ContextLatticeOrchestrator("http://127.0.0.1:8075", agent_id="codex_gpt5_test")
    orch.context_pack(
        query="codex preflight connectivity and retrieval",
        project="contextlattice",
        topic_path="runbooks/codex-integration",
    )
    post_calls = [c for c in dummy.calls if c[0] == "post" and c[1].endswith("/memory/context-pack")]
    assert post_calls, "expected /memory/context-pack call"
    payload = post_calls[-1][2] or {}
    assert payload.get("agent_id") == "codex_gpt5_test"
    assert payload.get("topic_path") == "runbooks/codex-integration"


def test_agent_preflight_uses_profile_defaults(monkeypatch):
    dummy = _DummyClient()
    monkeypatch.setattr(ao, "ContextLatticeClient", lambda *args, **kwargs: _DummyContextLatticeClient(dummy))
    orch = ao.ContextLatticeOrchestrator("http://127.0.0.1:8075", agent_id="codex_gpt5_test")
    payload = orch.agent_preflight(agent="claude-code", project="contextlattice")

    assert payload.get("agent") == "claude-code"
    assert payload.get("agent_id") == "claude_code_agent"
    search_calls = [c for c in dummy.calls if c[0] == "post" and c[1].endswith("/memory/search")]
    assert search_calls, "expected /memory/search calls"
    scoped_payload = search_calls[0][2] or {}
    assert scoped_payload.get("topic_path") == "runbooks/claude-code-integration"
    assert scoped_payload.get("agent_id") == "claude_code_agent"
    pack_calls = [c for c in dummy.calls if c[0] == "post" and c[1].endswith("/memory/context-pack")]
    assert pack_calls, "expected /memory/context-pack call"
    context_payload = pack_calls[0][2] or {}
    assert context_payload.get("agent_id") == "claude_code_agent"


def test_compaction_handoff_writes_and_reads_back(monkeypatch):
    dummy = _DummyClient()
    monkeypatch.setattr(ao, "ContextLatticeClient", lambda *args, **kwargs: _DummyContextLatticeClient(dummy))
    orch = ao.ContextLatticeOrchestrator("http://127.0.0.1:8075", agent_id="codex_gpt5_test")
    payload = orch.compaction_handoff(
        project="contextlattice",
        summary="keep objective continuity after compaction",
        topic_path="runbooks/context-compaction-handoff",
        retrieval_mode="balanced",
        query="context compaction handoff mission objective goal",
    )
    assert payload.get("ok") is True
    assert str(payload.get("file") or "").startswith("notes/compaction/")

    write_calls = [c for c in dummy.calls if c[0] == "post" and c[1].endswith("/memory/write")]
    assert write_calls, "expected /memory/write call"
    write_payload = write_calls[-1][2] or {}
    assert write_payload.get("projectName") == "contextlattice"
    assert write_payload.get("topicPath") == "runbooks/context-compaction-handoff"
    content = str(write_payload.get("content") or "")
    assert "Context Compaction Handoff" in content
    assert "keep objective continuity after compaction" in content

    search_calls = [c for c in dummy.calls if c[0] == "post" and c[1].endswith("/memory/search")]
    assert search_calls, "expected /memory/search readback"
