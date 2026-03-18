from __future__ import annotations

from pathlib import Path
import sys


REPO_ROOT = Path(__file__).resolve().parents[3]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from scripts.context_expansion_runtime import ContextExpansionRuntime, _slice_with_budget, _truncate_by_tokens


def test_truncate_and_slice_with_budget_is_deterministic():
    text = "abcdefghijklmnopqrstuvwxyz"
    clipped = _truncate_by_tokens(text, 2)
    assert clipped.endswith("…")
    assert len(clipped) <= 8

    items = [
        {"text": "alpha " * 40},
        {"text": "beta " * 40},
        {"text": "gamma " * 40},
    ]
    sliced = _slice_with_budget(items, lambda row: row["text"], token_budget=80)
    assert sliced
    assert sliced[0]["text"].startswith("alpha")
    # Deterministic ordering preserves first item first.
    assert len(sliced) >= 1


def test_prepare_runs_broaden_and_deep_escalation(monkeypatch):
    class DummyRuntime(ContextExpansionRuntime):
        def __init__(self):
            super().__init__("http://example.local", api_key="test", agent_id="test-agent")
            self.deep_escalation_enabled = True
            self.deep_poll_secs = 0.1
            self.calls: list[tuple[str, str | None, str, bool | None]] = []

        def _context_pack(self, *, query: str, project: str, topic_path: str | None, retrieval_mode: str):
            return {
                "query": query,
                "context_pack": {"facts": [], "numericFacts": [], "results": [], "warnings": []},
                "warnings": [],
            }

        def _search(
            self,
            *,
            query: str,
            project: str,
            topic_path: str | None,
            retrieval_mode: str,
            deep_async: bool | None = None,
        ):
            self.calls.append((query, topic_path, retrieval_mode, deep_async))
            if retrieval_mode == "deep":
                return {
                    "token": "tok-1",
                    "results": [],
                    "grounding": {"facts": [], "numeric_facts": []},
                    "retrieval_lifecycle": {"status": "running", "sources": {"pending": ["letta"]}},
                    "warnings": ["deep async started"],
                }
            if topic_path is not None:
                return {
                    "degraded": True,
                    "results": [{"project": project, "file": "a.md", "source": "qdrant", "summary": "one", "score": 0.9}],
                    "grounding": {"facts": [], "numeric_facts": []},
                    "retrieval_lifecycle": {"status": "partial", "sources": {"pending": ["letta"]}},
                    "warnings": ["partial"],
                }
            return {
                "degraded": False,
                "results": [
                    {"project": project, "file": "_rollups/topics/x.json", "source": "topic_rollups", "summary": "rollup", "score": 0.8}
                ],
                "grounding": {
                    "facts": [{"text": "broadened fact", "source": {"project": project, "file": "facts.md", "source": "qdrant"}}],
                    "numeric_facts": [{"field": "score", "value": 0.8}],
                },
                "retrieval_lifecycle": {"status": "partial", "sources": {"pending": ["mindsdb"]}},
                "warnings": ["broadened"],
            }

        def _request_json(self, method: str, path: str, payload=None, timeout=None):
            if path.startswith("/memory/search/continuations/"):
                return {
                    "status": "completed",
                    "result": {
                        "degraded": False,
                        "results": [
                            {"project": "demo", "file": "deep.md", "source": "letta", "summary": "deep fact", "score": 0.7}
                        ],
                        "grounding": {
                            "facts": [{"text": "deep completed", "source": {"project": "demo", "file": "deep.md", "source": "letta"}}],
                            "numeric_facts": [],
                        },
                        "retrieval_lifecycle": {"status": "succeeded", "sources": {"returned_now": ["letta"]}},
                        "warnings": [],
                    },
                }
            raise AssertionError(f"unexpected call path: {path}")

    runtime = DummyRuntime()
    task = {
        "id": "task-1",
        "title": "Need context expansion",
        "project": "demo",
        "payload": {"topic_path": "architecture/context-expansion", "tools": ["planner"]},
    }
    bundle = runtime.prepare(task)
    assert bundle["expansion"]["broadened_scope"] is True
    assert bundle["expansion"]["deep_escalated"] is True
    assert bundle["layers"]["l0_facts"]
    assert bundle["layers"]["l1_rollups"]
    assert "planner" in bundle["tool_slices"]
    prompt = runtime.render_for_prompt(bundle)
    assert "Context Expansion Pack" in prompt
    assert "L0 Facts" in prompt
