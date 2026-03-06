from __future__ import annotations

import asyncio
import importlib.util
import json
import sys
import time
from types import SimpleNamespace
from datetime import datetime
from pathlib import Path
from typing import Any

import pytest
from starlette.requests import Request


def _load_orchestrator_module():
    app_path = Path(__file__).resolve().parents[1] / "app.py"
    spec = importlib.util.spec_from_file_location("orchestrator_app_test", app_path)
    if spec is None or spec.loader is None:
        raise RuntimeError("Unable to load orchestrator app module")
    module = importlib.util.module_from_spec(spec)
    # Pydantic forward refs resolve via module globals from sys.modules.
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


orchestrator = _load_orchestrator_module()


def test_parse_letta_archival_content():
    payload = (
        "project=alpha file=decisions/one.md topic=agents/protocols\n"
        "summary: Key decision made for retrieval path\n"
        "metadata: {\"kind\":\"decision\"}"
    )
    parsed = orchestrator._parse_letta_archival_content(payload)
    assert parsed["project"] == "alpha"
    assert parsed["file"] == "decisions/one.md"
    assert parsed["topic_path"] == "agents/protocols"
    assert "retrieval path" in parsed["summary"]


def test_mindsdb_rows_from_table_format():
    raw = {
        "type": "table",
        "column_names": ["project", "file", "summary"],
        "data": [["alpha", "notes/a.txt", "hello"]],
    }
    rows = orchestrator._mindsdb_rows(raw)
    assert rows == [{"project": "alpha", "file": "notes/a.txt", "summary": "hello"}]


def test_merge_federated_rows_applies_learning_adjustment():
    rows = {
        "mongo_raw": [
            {
                "project": "alpha",
                "file": "notes/a.txt",
                "summary": "prefer structured output for retrieval",
                "score": 0.4,
            }
        ]
    }
    merged = orchestrator._merge_federated_rows(
        rows,
        {"mongo_raw": 1.0},
        {"structured", "retrieval"},
        set(),
        learning_enabled=True,
    )
    assert len(merged) == 1
    assert merged[0]["learning_adjustment"] > 0
    assert merged[0]["score"] > merged[0]["base_score"]


def test_merge_federated_rows_applies_fusion_quality_and_lifecycle_adjustments(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_FUSION_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_FUSION_LEXICAL_BOOST", 0.2)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_FUSION_CONSENSUS_BOOST", 0.05)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_FUSION_NUMERIC_MATCH_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_FUSION_NUMERIC_MATCH_BOOST", 0.1)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_FUSION_NUMERIC_MISS_PENALTY", 0.04)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LIFECYCLE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LIFECYCLE_REUSE_WEIGHT", 0.06)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LIFECYCLE_RECENCY_WEIGHT", 0.08)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LIFECYCLE_CONTRADICTION_PENALTY", 0.05)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LIFECYCLE_MAX_ADJUSTMENT", 0.4)
    now = time.monotonic()
    key = "alpha:notes/a.md"
    rows = {
        "qdrant": [
            {
                "project": "alpha",
                "file": "notes/a.md",
                "summary": "Win rate reached 88.1% after retrieval tuning",
                "score": 0.6,
            }
        ],
        "letta": [
            {
                "project": "alpha",
                "file": "notes/a.md",
                "summary": "Historical win rate reached 88.1%",
                "score": 0.59,
            }
        ],
    }
    merged = orchestrator._merge_federated_rows(
        rows,
        {"qdrant": 1.0, "letta": 1.0},
        set(),
        set(),
        learning_enabled=False,
        query="alpha win rate 88.1%",
        source_quality_multipliers={"qdrant": 1.0, "letta": 0.8},
        lifecycle_snapshot={
            key: {
                "hits": 12,
                "contradictions": 0,
                "last_seen_monotonic": now,
                "first_seen_monotonic": now - 60.0,
            }
        },
    )
    assert len(merged) == 1
    row = merged[0]
    assert row["score"] > row["base_score"]
    assert row["fusion_adjustment"] > 0
    assert row["numeric_adjustment"] > 0
    assert row["consensus_adjustment"] > 0
    assert row["lifecycle_adjustment"] >= 0
    assert sorted(row["sources"]) == ["letta", "qdrant"]


@pytest.mark.asyncio
async def test_retrieval_source_quality_snapshot_penalizes_unstable_sources(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SOURCE_QUALITY_ADAPTIVE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SOURCE_QUALITY_MIN_REQUESTS", 5)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SOURCE_QUALITY_MIN_MULTIPLIER", 0.6)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SOURCE_QUALITY_MAX_MULTIPLIER", 1.1)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SOURCE_QUALITY_TIMEOUT_WEIGHT", 0.55)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SOURCE_QUALITY_ERROR_WEIGHT", 0.45)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SOURCE_QUALITY_STEADY_BOOST", 0.03)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SOURCE_QUALITY_STEADY_TIMEOUT_RATE", 0.02)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SOURCE_QUALITY_STEADY_ERROR_RATE", 0.03)
    async with orchestrator.retrieval_latency_lock:
        orchestrator.retrieval_source_request_counts.clear()
        orchestrator.retrieval_source_error_counts.clear()
        orchestrator.retrieval_source_timeout_counts.clear()
        orchestrator.retrieval_source_request_counts["letta"] = 20
        orchestrator.retrieval_source_error_counts["letta"] = 8
        orchestrator.retrieval_source_timeout_counts["letta"] = 9
        orchestrator.retrieval_source_request_counts["qdrant"] = 20
        orchestrator.retrieval_source_error_counts["qdrant"] = 0
        orchestrator.retrieval_source_timeout_counts["qdrant"] = 0

    snapshot = await orchestrator._retrieval_source_quality_snapshot(
        sources=["letta", "qdrant"]
    )
    multipliers = snapshot["multipliers"]
    assert multipliers["letta"] < 1.0
    assert multipliers["qdrant"] >= 1.0
    assert multipliers["letta"] < multipliers["qdrant"]


@pytest.mark.asyncio
async def test_record_retrieval_lifecycle_observation_tracks_hits_and_contradictions(
    monkeypatch: pytest.MonkeyPatch,
):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LIFECYCLE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LIFECYCLE_MAX_KEYS", 128)
    row = {
        "project": "alpha",
        "file": "notes/a.md",
        "summary": "win rate at 88%",
        "score": 0.5,
    }
    async with orchestrator.retrieval_lifecycle_lock:
        orchestrator.retrieval_result_lifecycle.clear()

    await orchestrator._record_retrieval_lifecycle_observation(
        query="win rate should remain at 91%",
        results=[row],
    )
    snapshot = await orchestrator._retrieval_lifecycle_snapshot()
    key = orchestrator._result_identity(row)
    assert snapshot[key]["hits"] == 1
    assert snapshot[key]["contradictions"] == 1

    await orchestrator._record_retrieval_lifecycle_observation(
        query="win rate should remain at 91%",
        results=[{**row, "summary": "win rate at 91%"}],
    )
    snapshot = await orchestrator._retrieval_lifecycle_snapshot()
    assert snapshot[key]["hits"] == 2
    assert snapshot[key]["contradictions"] == 1


@pytest.mark.asyncio
async def test_federated_search_degrades_when_qdrant_fails(monkeypatch: pytest.MonkeyPatch):
    async def _qdrant(*args, **kwargs):
        raise RuntimeError("qdrant unavailable")

    async def _mongo(*args, **kwargs):
        return [
            {
                "project": "alpha",
                "file": "notes/a.txt",
                "summary": "alpha memory entry",
                "score": 0.45,
                "source": "mongo_raw",
            }
        ]

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _mongo)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _empty)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)

    results, debug, warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=["qdrant", "mongo_raw"],
        preferences={"positive": ["alpha"]},
        rerank_with_learning=True,
    )
    assert len(results) == 1
    assert results[0]["project"] == "alpha"
    assert debug["source_errors"].get("qdrant")
    assert any("qdrant retrieval failed" in item for item in warnings)


@pytest.mark.asyncio
async def test_federated_search_explicit_sources_do_not_skip_slow_batch(monkeypatch: pytest.MonkeyPatch):
    async def _qdrant(*args, **kwargs):
        return [
            {
                "project": "alpha",
                "file": f"fast/{idx}.txt",
                "summary": "high confidence fast source row",
                "score": 0.95 - (idx * 0.01),
                "source": "qdrant",
            }
            for idx in range(12)
        ]

    async def _letta(*args, **kwargs):
        return [
            {
                "project": "alpha",
                "file": "slow/letta.md",
                "summary": "slow source still requested explicitly",
                "score": 0.4,
                "source": "letta",
            }
        ]

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_FAST_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_QDRANT],
    )
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_SLOW_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_LETTA],
    )

    results, debug, warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=["qdrant", "letta"],
        preferences=None,
        rerank_with_learning=False,
    )
    assert results
    assert debug["source_counts"]["letta"] == 1
    assert debug["staged_fetch"]["slow_sources_skipped"] == []
    assert debug["staged_fetch"]["explicit_source_override"] is True
    assert warnings == []


@pytest.mark.asyncio
async def test_search_letta_archival_applies_top_k_cap_and_cache(monkeypatch: pytest.MonkeyPatch):
    class _FakeResponse:
        def __init__(self, body: dict[str, Any]):
            self.status_code = 200
            self._body = body
            self.content = b"{}"
            self.text = json.dumps(body)

        def json(self):
            return self._body

    class _FakeClient:
        def __init__(self):
            self.calls = 0
            self.last_params: dict[str, Any] | None = None

        async def get(self, _url: str, params: dict[str, Any], headers: dict[str, str], timeout: float):
            self.calls += 1
            self.last_params = dict(params)
            return _FakeResponse(
                {
                    "results": [
                        {
                            "id": "passage-1",
                            "content": (
                                "project=alpha file=notes/a.md topic=decisions\n"
                                "summary: win rate reached 88.1%"
                            ),
                            "timestamp": "2026-03-02T18:00:00Z",
                        }
                    ]
                }
            )

    fake_client = _FakeClient()

    async def _resolve(_session_id: str, _headers: dict[str, str]) -> str:
        return "agent-test"

    async def _client() -> _FakeClient:
        return fake_client

    monkeypatch.setattr(orchestrator, "_resolve_letta_agent_id", _resolve)
    monkeypatch.setattr(orchestrator, "_get_letta_client", _client)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_TOP_K_FACTOR", 2.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_TOP_K_CAP", 5)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_CACHE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_CACHE_TTL_SECS", 60.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_CACHE_MAX_KEYS", 32)
    monkeypatch.setattr(orchestrator, "letta_search_cache_hits", 0)
    monkeypatch.setattr(orchestrator, "letta_search_cache_misses", 0)
    monkeypatch.setattr(orchestrator, "letta_search_cache_evictions", 0)
    async with orchestrator.letta_search_cache_lock:
        orchestrator.letta_search_cache.clear()

    first = await orchestrator.search_letta_archival("win rate", limit=10, project_filter="alpha")
    second = await orchestrator.search_letta_archival("win rate", limit=10, project_filter="alpha")

    assert first
    assert second
    assert fake_client.calls == 1
    assert fake_client.last_params is not None
    assert fake_client.last_params["top_k"] == 5
    assert orchestrator.letta_search_cache_hits >= 1


@pytest.mark.asyncio
async def test_search_letta_archival_timeout_warms_cache_async(monkeypatch: pytest.MonkeyPatch):
    class _FakeResponse:
        def __init__(self, body: dict[str, Any]):
            self.status_code = 200
            self._body = body
            self.content = b"{}"
            self.text = json.dumps(body)

        def json(self):
            return self._body

    class _FakeClient:
        def __init__(self):
            self.calls: list[float] = []

        async def get(self, _url: str, params: dict[str, Any], headers: dict[str, str], timeout: float):
            self.calls.append(float(timeout))
            if len(self.calls) == 1:
                raise orchestrator.httpx.ReadTimeout("timed out", request=None)
            return _FakeResponse(
                {
                    "results": [
                        {
                            "id": "passage-async",
                            "content": (
                                "project=alpha file=notes/async.md topic=retrieval/cache\n"
                                "summary: async warm cache response"
                            ),
                            "timestamp": "2026-03-04T22:00:00Z",
                        }
                    ]
                }
            )

    fake_client = _FakeClient()

    async def _resolve(_session_id: str, _headers: dict[str, str]) -> str:
        return "agent-test"

    async def _client() -> _FakeClient:
        return fake_client

    monkeypatch.setattr(orchestrator, "_resolve_letta_agent_id", _resolve)
    monkeypatch.setattr(orchestrator, "_get_letta_client", _client)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_TOP_K_FACTOR", 2.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_TOP_K_CAP", 5)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_TIMEOUT_SECS", 2.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_CACHE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_CACHE_TTL_SECS", 60.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_CACHE_MAX_KEYS", 32)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_ASYNC_WARM_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_ASYNC_WARM_TIMEOUT_SECS", 12.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_ASYNC_WARM_MAX_INFLIGHT", 4)
    monkeypatch.setattr(orchestrator, "letta_search_warm_started", 0)
    monkeypatch.setattr(orchestrator, "letta_search_warm_completed", 0)
    monkeypatch.setattr(orchestrator, "letta_search_warm_failed", 0)
    async with orchestrator.letta_search_cache_lock:
        orchestrator.letta_search_cache.clear()
    async with orchestrator.letta_search_warm_lock:
        orchestrator.letta_search_warm_inflight.clear()

    first = await orchestrator.search_letta_archival("warm cache", limit=5, project_filter="alpha")
    assert first == []

    cache_key = orchestrator._letta_search_cache_key(
        query="warm cache",
        limit=5,
        project_filter="alpha",
        topic_filter=None,
        top_k=5,
    )
    warmed = None
    for _ in range(80):
        warmed = await orchestrator._letta_search_cache_get(cache_key)
        if warmed:
            break
        await asyncio.sleep(0.01)

    assert warmed
    assert orchestrator.letta_search_warm_started >= 1
    assert orchestrator.letta_search_warm_completed >= 1
    assert fake_client.calls[0] == 2.0
    assert max(fake_client.calls) >= 12.0

    second = await orchestrator.search_letta_archival("warm cache", limit=5, project_filter="alpha")
    assert second
    assert len(fake_client.calls) == 2


@pytest.mark.asyncio
async def test_federated_search_fast_mode_uses_fast_sources(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0, "mongo_raw": 0, "mindsdb": 0, "topic_rollups": 0, "letta": 0, "memory_bank": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [{"project": "alpha", "file": "fast/a.md", "summary": "fast result", "score": 0.7, "source": "qdrant"}]

    async def _mongo(*args, **kwargs):
        calls["mongo_raw"] += 1
        return []

    async def _mindsdb(*args, **kwargs):
        calls["mindsdb"] += 1
        return []

    async def _rollups(*args, **kwargs):
        calls["topic_rollups"] += 1
        return []

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return []

    async def _memory_bank(*args, **kwargs):
        calls["memory_bank"] += 1
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _mongo)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _mindsdb)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _rollups)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _memory_bank)
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_FAST_SOURCES",
        [
            orchestrator.RETRIEVAL_SOURCE_QDRANT,
            orchestrator.RETRIEVAL_SOURCE_MONGO_RAW,
            orchestrator.RETRIEVAL_SOURCE_MINDSDB,
            orchestrator.RETRIEVAL_SOURCE_TOPIC_ROLLUPS,
        ],
    )

    results, debug, warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="fast",
    )

    assert results
    assert warnings == []
    assert calls["qdrant"] == 1
    assert calls["mongo_raw"] == 1
    assert calls["mindsdb"] == 1
    assert calls["topic_rollups"] == 1
    assert calls["letta"] == 0
    assert calls["memory_bank"] == 0
    assert debug["retrieval_mode"] == "fast"
    assert debug["sources"] == [
        orchestrator.RETRIEVAL_SOURCE_QDRANT,
        orchestrator.RETRIEVAL_SOURCE_MONGO_RAW,
        orchestrator.RETRIEVAL_SOURCE_MINDSDB,
        orchestrator.RETRIEVAL_SOURCE_TOPIC_ROLLUPS,
    ]


@pytest.mark.asyncio
async def test_federated_search_deep_mode_includes_slow_sources_for_explicit_override(
    monkeypatch: pytest.MonkeyPatch,
):
    calls = {"qdrant": 0, "letta": 0, "memory_bank": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [
            {
                "project": "alpha",
                "file": f"fast/{idx}.md",
                "summary": "high-confidence answer from fast source",
                "score": 0.96 - (idx * 0.01),
                "source": "qdrant",
            }
            for idx in range(8)
        ]

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return [{"project": "alpha", "file": "slow/letta.md", "summary": "slow row", "score": 0.4, "source": "letta"}]

    async def _memory_bank(*args, **kwargs):
        calls["memory_bank"] += 1
        return []

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _memory_bank)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_RESULTS", 1)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_TOP_SCORE", 0.4)
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_FAST_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_QDRANT],
    )
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_SLOW_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_LETTA, orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK],
    )

    results, debug, _ = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT, orchestrator.RETRIEVAL_SOURCE_LETTA, orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK],
        rerank_with_learning=False,
        retrieval_mode="deep",
    )

    assert results
    assert calls["qdrant"] == 1
    assert calls["letta"] == 1
    assert calls["memory_bank"] == 1
    assert debug["retrieval_mode"] == "deep"
    assert debug["staged_fetch"]["force_include_slow"] is False
    assert debug["staged_fetch"]["explicit_source_override"] is True
    assert debug["staged_fetch"]["slow_sources_skipped"] == []


@pytest.mark.asyncio
async def test_federated_search_deep_mode_does_not_force_degraded_slow_sources(
    monkeypatch: pytest.MonkeyPatch,
):
    calls = {"qdrant": 0, "letta": 0, "memory_bank": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [
            {
                "project": "alpha",
                "file": f"fast/{idx}.md",
                "summary": "high-confidence answer from fast source",
                "score": 0.97 - (idx * 0.01),
                "source": "qdrant",
            }
            for idx in range(8)
        ]

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return [{"project": "alpha", "file": "slow/letta.md", "summary": "slow row", "score": 0.4, "source": "letta"}]

    async def _memory_bank(*args, **kwargs):
        calls["memory_bank"] += 1
        return []

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _memory_bank)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_RESULTS", 1)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_TOP_SCORE", 0.4)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_DIVERSITY", 1)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_STABILITY_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_STABILITY_MIN_REQUESTS", 10)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_TIMEOUT_RATE_THRESHOLD", 0.5)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_ERROR_RATE_THRESHOLD", 0.6)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_COOLDOWN_SECS", 180.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_DEGRADED_TIMEOUT_SECS", 12.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_DEGRADED_TIMEOUT_SECS", 2.5)
    monkeypatch.setattr(
        orchestrator,
        "RETRIEVAL_SOURCES_ENV",
        ",".join(
            [
                orchestrator.RETRIEVAL_SOURCE_QDRANT,
                orchestrator.RETRIEVAL_SOURCE_LETTA,
                orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK,
            ]
        ),
    )
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_FAST_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_QDRANT],
    )
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_SLOW_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_LETTA, orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK],
    )
    async with orchestrator.retrieval_latency_lock:
        orchestrator.retrieval_source_request_counts.clear()
        orchestrator.retrieval_source_error_counts.clear()
        orchestrator.retrieval_source_timeout_counts.clear()
        orchestrator.retrieval_slow_source_cooldown_until.clear()
        orchestrator.retrieval_source_request_counts[orchestrator.RETRIEVAL_SOURCE_LETTA] = 20
        orchestrator.retrieval_source_timeout_counts[orchestrator.RETRIEVAL_SOURCE_LETTA] = 16
        orchestrator.retrieval_source_request_counts[orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK] = 20
        orchestrator.retrieval_source_timeout_counts[orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK] = 12

    results, debug, _ = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="deep",
    )

    assert results
    assert calls["qdrant"] == 1
    assert calls["letta"] == 0
    assert calls["memory_bank"] == 0
    assert debug["retrieval_mode"] == "deep"
    assert debug["staged_fetch"]["force_include_slow"] is False
    assert debug["staged_fetch"]["slow_sources_skipped"] == [
        orchestrator.RETRIEVAL_SOURCE_LETTA,
        orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK,
    ]
    assert orchestrator.RETRIEVAL_SOURCE_LETTA in debug["source_policy"]["degraded_sources"]
    assert orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK in debug["source_policy"]["degraded_sources"]


@pytest.mark.asyncio
async def test_federated_search_pathway_cache_hits(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [{"project": "alpha", "file": "notes/a.md", "summary": "cached result", "score": 0.8, "source": "qdrant"}]

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_TTL_SECS", 120.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_MAX_KEYS", 128)
    monkeypatch.setattr(orchestrator, "retrieval_pathway_cache_hits", 0)
    monkeypatch.setattr(orchestrator, "retrieval_pathway_cache_misses", 0)
    monkeypatch.setattr(orchestrator, "retrieval_pathway_cache_evictions", 0)
    async with orchestrator.retrieval_pathway_cache_lock:
        orchestrator.retrieval_pathway_cache.clear()

    first, debug_first, _ = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT],
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )
    second, debug_second, _ = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT],
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )

    assert first
    assert second
    assert calls["qdrant"] == 1
    assert debug_first["cache"]["pathway_hit"] is False
    assert debug_second["cache"]["pathway_hit"] is True
    assert orchestrator.retrieval_pathway_cache_hits >= 1


@pytest.mark.asyncio
async def test_retrieval_pathway_cache_reads_backend_on_memory_miss(monkeypatch: pytest.MonkeyPatch):
    backend_calls = {"get": 0}

    async def _backend_get(_key: str):
        backend_calls["get"] += 1
        return (
            [{"project": "alpha", "file": "notes/backend.md", "summary": "backend hit", "score": 0.6}],
            {"cache": {"pathway_hit": False}},
            [],
        )

    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_TTL_SECS", 120.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_MAX_KEYS", 128)
    monkeypatch.setattr(orchestrator, "_retrieval_pathway_cache_backend_get", _backend_get)
    monkeypatch.setattr(orchestrator, "retrieval_pathway_cache_hits", 0)
    monkeypatch.setattr(orchestrator, "retrieval_pathway_cache_misses", 0)
    async with orchestrator.retrieval_pathway_cache_lock:
        orchestrator.retrieval_pathway_cache.clear()

    first = await orchestrator._retrieval_pathway_cache_get("abc123")
    second = await orchestrator._retrieval_pathway_cache_get("abc123")

    assert first is not None
    assert second is not None
    assert backend_calls["get"] == 1
    assert orchestrator.retrieval_pathway_cache_hits >= 2


@pytest.mark.asyncio
async def test_retrieval_pathway_cache_set_writes_backend(monkeypatch: pytest.MonkeyPatch):
    calls = {"set": 0}

    async def _backend_set(_key: str, **kwargs):
        calls["set"] += 1
        assert kwargs["results"]

    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_TTL_SECS", 120.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_MAX_KEYS", 128)
    monkeypatch.setattr(orchestrator, "_retrieval_pathway_cache_backend_set", _backend_set)
    async with orchestrator.retrieval_pathway_cache_lock:
        orchestrator.retrieval_pathway_cache.clear()

    await orchestrator._retrieval_pathway_cache_set(
        "write-key",
        results=[{"project": "alpha", "file": "notes/a.md", "summary": "cached", "score": 0.7}],
        retrieval_debug={"cache": {"pathway_hit": False}},
        warnings=[],
    )
    assert calls["set"] == 1


@pytest.mark.asyncio
async def test_retrieval_latency_snapshot_reports_percentiles(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LATENCY_HISTORY_LIMIT", 128)
    async with orchestrator.retrieval_latency_lock:
        orchestrator.retrieval_source_latency_samples.clear()
        orchestrator.retrieval_source_request_counts.clear()
        orchestrator.retrieval_source_error_counts.clear()
        orchestrator.retrieval_source_timeout_counts.clear()
        orchestrator.retrieval_latency_mode_counts.clear()
        orchestrator.retrieval_latency_updated_at = None

    await orchestrator._record_retrieval_source_latency(
        source="letta",
        duration_ms=10.0,
        ok=True,
        timed_out=False,
        retrieval_mode="balanced",
    )
    await orchestrator._record_retrieval_source_latency(
        source="letta",
        duration_ms=20.0,
        ok=True,
        timed_out=False,
        retrieval_mode="balanced",
    )
    await orchestrator._record_retrieval_source_latency(
        source="letta",
        duration_ms=40.0,
        ok=False,
        timed_out=True,
        retrieval_mode="deep",
    )

    snapshot = await orchestrator._retrieval_latency_snapshot()
    letta = snapshot["sources"]["letta"]
    assert letta["samples"] == 3
    assert letta["requests"] == 3
    assert letta["errors"] == 1
    assert letta["timeouts"] == 1
    assert letta["p99Ms"] >= letta["p95Ms"] >= letta["p50Ms"] >= letta["minMs"]
    assert snapshot["modes"]["balanced"] == 2
    assert snapshot["modes"]["deep"] == 1


@pytest.mark.asyncio
async def test_retrieval_slow_source_runtime_policy_marks_degraded_sources(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_STABILITY_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_STABILITY_MIN_REQUESTS", 10)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_TIMEOUT_RATE_THRESHOLD", 0.5)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_ERROR_RATE_THRESHOLD", 0.6)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_COOLDOWN_SECS", 180.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LETTA_DEGRADED_TIMEOUT_SECS", 12.0)
    async with orchestrator.retrieval_latency_lock:
        orchestrator.retrieval_source_request_counts.clear()
        orchestrator.retrieval_source_error_counts.clear()
        orchestrator.retrieval_source_timeout_counts.clear()
        orchestrator.retrieval_slow_source_cooldown_until.clear()
        orchestrator.retrieval_source_request_counts[orchestrator.RETRIEVAL_SOURCE_LETTA] = 20
        orchestrator.retrieval_source_error_counts[orchestrator.RETRIEVAL_SOURCE_LETTA] = 8
        orchestrator.retrieval_source_timeout_counts[orchestrator.RETRIEVAL_SOURCE_LETTA] = 12

    policy = await orchestrator._retrieval_slow_source_runtime_policy(
        sources=[orchestrator.RETRIEVAL_SOURCE_LETTA],
        retrieval_mode="balanced",
    )
    assert policy["enabled"] is True
    assert orchestrator.RETRIEVAL_SOURCE_LETTA in policy["degraded"]
    assert policy["timeout_overrides"][orchestrator.RETRIEVAL_SOURCE_LETTA] == 12.0


@pytest.mark.asyncio
async def test_retrieval_slow_source_runtime_policy_skips_caps_for_explicit_sources(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_STABILITY_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_STABILITY_MIN_REQUESTS", 10)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_TIMEOUT_RATE_THRESHOLD", 0.5)
    async with orchestrator.retrieval_latency_lock:
        orchestrator.retrieval_source_request_counts.clear()
        orchestrator.retrieval_source_error_counts.clear()
        orchestrator.retrieval_source_timeout_counts.clear()
        orchestrator.retrieval_slow_source_cooldown_until.clear()
        orchestrator.retrieval_source_request_counts[orchestrator.RETRIEVAL_SOURCE_LETTA] = 20
        orchestrator.retrieval_source_timeout_counts[orchestrator.RETRIEVAL_SOURCE_LETTA] = 16

    policy = await orchestrator._retrieval_slow_source_runtime_policy(
        sources=[orchestrator.RETRIEVAL_SOURCE_LETTA],
        retrieval_mode="balanced",
        explicit_source_override=True,
    )
    assert policy["explicit_source_override"] is True
    assert policy["degraded"] == {}
    assert policy["timeout_overrides"] == {}


@pytest.mark.asyncio
async def test_federated_search_staged_fetch_requires_fast_source_diversity(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0, "mongo_raw": 0, "letta": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [
            {
                "project": "alpha",
                "file": f"fast/{idx}.md",
                "summary": "high-confidence answer from fast source",
                "score": 0.98 - (idx * 0.01),
                "source": "qdrant",
            }
            for idx in range(10)
        ]

    async def _mongo(*args, **kwargs):
        calls["mongo_raw"] += 1
        return []

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return [{"project": "alpha", "file": "slow/letta.md", "summary": "slow row", "score": 0.41, "source": "letta"}]

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _mongo)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_RESULTS", 3)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_TOP_SCORE", 0.6)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_DIVERSITY", 2)
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_FAST_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_QDRANT, orchestrator.RETRIEVAL_SOURCE_MONGO_RAW],
    )
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_SLOW_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_LETTA],
    )
    async with orchestrator.retrieval_latency_lock:
        orchestrator.retrieval_source_request_counts.clear()
        orchestrator.retrieval_source_error_counts.clear()
        orchestrator.retrieval_source_timeout_counts.clear()
        orchestrator.retrieval_slow_source_cooldown_until.clear()

    _, debug, _ = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT, orchestrator.RETRIEVAL_SOURCE_MONGO_RAW, orchestrator.RETRIEVAL_SOURCE_LETTA],
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )

    assert calls["qdrant"] == 1
    assert calls["mongo_raw"] == 1
    assert calls["letta"] == 1
    assert debug["staged_fetch"]["slow_sources_skipped"] == []
    assert debug["staged_fetch"]["slow_source_min_diversity"] == 2


@pytest.mark.asyncio
async def test_federated_search_passes_timeout_budget_to_slow_sources(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, float] = {}

    async def _letta(*args, **kwargs):
        captured["timeout_secs"] = float(kwargs.get("timeout_secs") or 0.0)
        return []

    async def _memory_bank(*args, **kwargs):
        captured["time_budget_secs"] = float(kwargs.get("time_budget_secs") or 0.0)
        return []

    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _memory_bank)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", False)
    async with orchestrator.retrieval_latency_lock:
        orchestrator.retrieval_source_request_counts.clear()
        orchestrator.retrieval_source_error_counts.clear()
        orchestrator.retrieval_source_timeout_counts.clear()
        orchestrator.retrieval_slow_source_cooldown_until.clear()

    await orchestrator.federated_search_memory(
        "alpha",
        limit=4,
        sources=[orchestrator.RETRIEVAL_SOURCE_LETTA, orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK],
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )

    assert captured.get("timeout_secs", 0.0) > 0.0
    assert captured.get("time_budget_secs", 0.0) > 0.0


def test_build_retrieval_alerts_flags_letta_and_warmer(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ALERTS_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ALERT_MIN_REQUESTS", 2)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ALERT_LETTA_P95_MS", 1000.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ALERT_LETTA_P99_MS", 1500.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ALERT_LETTA_TIMEOUT_RATE", 0.2)
    orchestrator.retrieval_pathway_warmer_state["lastError"] = "warm cycle failed"
    orchestrator.retrieval_pathway_warmer_state["lastResult"] = {"errors": {"alpha": "timeout"}}

    alerts = orchestrator._build_retrieval_alerts(
        {
            "sources": {
                "letta": {
                    "requests": 10,
                    "timeouts": 3,
                    "p95Ms": 1800.0,
                    "p99Ms": 2100.0,
                }
            }
        }
    )
    codes = {item.get("code") for item in alerts["active"]}
    assert alerts["enabled"] is True
    assert "letta_p95_high" in codes
    assert "letta_p99_high" in codes
    assert "letta_timeout_rate_high" in codes
    assert "retrieval_warmer_last_error" in codes


def test_build_retrieval_alerts_disabled(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ALERTS_ENABLED", False)
    alerts = orchestrator._build_retrieval_alerts({})
    assert alerts["enabled"] is False
    assert alerts["active"] == []
    assert alerts["count"] == 0


@pytest.mark.asyncio
async def test_warm_retrieval_pathways_uses_top_observed_queries(monkeypatch: pytest.MonkeyPatch):
    warmed_queries: list[str] = []

    async def _runner(entry: dict[str, Any]) -> bool:
        warmed_queries.append(str(entry.get("query")))
        return True

    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_WARMER_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_WARMER_TOP_QUERIES", 2)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_STATS_TTL_SECS", 3600.0)
    monkeypatch.setattr(orchestrator, "_run_retrieval_pathway_warm_query", _runner)
    async with orchestrator.retrieval_pathway_stats_lock:
        orchestrator.retrieval_pathway_stats.clear()

    await orchestrator._record_retrieval_pathway_observation(
        query="alpha route",
        project_filter="alpha",
        topic_filter=None,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT],
        source_weights={"qdrant": 1.0},
        retrieval_mode="balanced",
    )
    await orchestrator._record_retrieval_pathway_observation(
        query="alpha route",
        project_filter="alpha",
        topic_filter=None,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT],
        source_weights={"qdrant": 1.0},
        retrieval_mode="balanced",
    )
    await orchestrator._record_retrieval_pathway_observation(
        query="beta route",
        project_filter="beta",
        topic_filter=None,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT],
        source_weights={"qdrant": 1.0},
        retrieval_mode="deep",
    )

    result = await orchestrator._warm_retrieval_pathways_once()
    assert result["enabled"] is True
    assert result["candidates"] == 2
    assert result["warmed"] == 2
    assert warmed_queries[0] == "alpha route"
    assert "beta route" in warmed_queries


def test_validate_security_posture_requires_api_key_in_production(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "MEMMCP_ENV", "production")
    monkeypatch.setattr(orchestrator, "ORCH_SECURITY_STRICT", True)
    monkeypatch.setattr(orchestrator, "ORCH_API_KEY", "")
    monkeypatch.setattr(orchestrator, "ORCH_PUBLIC_STATUS", False)
    monkeypatch.setattr(orchestrator, "ORCH_PUBLIC_DOCS", False)
    with pytest.raises(RuntimeError):
        orchestrator.validate_orchestrator_security_posture()


def test_extract_api_key_accepts_query_param():
    scope = {
        "type": "http",
        "method": "GET",
        "path": "/telemetry/trading",
        "headers": [],
        "query_string": b"api_key=query-secret",
    }
    request = Request(scope)
    assert orchestrator._extract_api_key(request) == "query-secret"


def test_extract_api_key_prefers_header_over_query():
    scope = {
        "type": "http",
        "method": "GET",
        "path": "/telemetry/trading",
        "headers": [(b"x-api-key", b"header-secret")],
        "query_string": b"api_key=query-secret",
    }
    request = Request(scope)
    assert orchestrator._extract_api_key(request) == "header-secret"


def test_prepare_content_for_storage_redacts_in_redact_mode(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "SECRETS_STORAGE_MODE", "redact")
    content = "api_key=sk-1234567890abcdefghijklmno"
    stored, warning = orchestrator._prepare_content_for_storage(content)
    assert stored != content
    assert "[REDACTED]" in stored
    assert warning
    assert "redacted" in warning


def test_prepare_content_for_storage_blocks_in_block_mode(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "SECRETS_STORAGE_MODE", "block")
    with pytest.raises(orchestrator.HTTPException) as exc:
        orchestrator._prepare_content_for_storage("api_key=sk-1234567890abcdefghijklmno")
    assert exc.value.status_code == 422


def test_prepare_content_for_storage_allows_in_allow_mode(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "SECRETS_STORAGE_MODE", "allow")
    content = "api_key=sk-1234567890abcdefghijklmno"
    stored, warning = orchestrator._prepare_content_for_storage(content)
    assert stored == content
    assert warning is None


@pytest.mark.asyncio
async def test_memory_search_fails_open_when_preference_store_unavailable(
    monkeypatch: pytest.MonkeyPatch,
):
    async def _raise_feedback(*args, **kwargs):
        raise RuntimeError("disk I/O error")

    async def _pipeline(*args, **kwargs):
        return (
            [],
            {"source_errors": {}, "source_counts": {}, "resolved_sources": []},
            [],
            {"strict_numeric_copy": True, "facts": [], "numeric_facts": []},
        )

    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", True)
    monkeypatch.setattr(orchestrator, "list_feedback_records", _raise_feedback)
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)

    response = await orchestrator.search_memory(orchestrator.MemorySearch(query="alpha"))
    assert response["results"] == []
    assert response["grounding"]["strict_numeric_copy"] is True
    assert any("Preference context unavailable" in warning for warning in response["warnings"])


@pytest.mark.asyncio
async def test_memory_search_uses_agent_profile_pipeline_and_grounding(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, Any] = {}

    def _resolve(agent_id: str | None) -> dict[str, Any]:
        assert agent_id == "trader"
        return {
            "retrieval_mode": "balanced",
            "sources": ["mongo_raw", "letta"],
            "source_weights": {"letta": 1.1},
            "default_project": "alpha",
            "topic_prefixes": ["strategy/live"],
            "auto_escalate": True,
            "query_expansion": True,
            "escalate_min_results": 3,
            "escalate_min_top_score": 0.6,
        }

    async def _pipeline(**kwargs):
        captured.update(kwargs)
        return (
            [
                {
                    "project": "alpha",
                    "file": "notes/a.md",
                    "summary": "PnL improved to $1,200",
                    "score": 0.91,
                    "source": "mongo_raw",
                }
            ],
            {"retrieval_mode": "deep", "source_errors": {}, "source_counts": {"mongo_raw": 1}},
            [],
            {
                "strict_numeric_copy": True,
                "facts": [],
                "numeric_facts": [{"value": "$1,200", "verbatim": True}],
            },
        )

    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", False)
    monkeypatch.setattr(orchestrator, "_resolve_agent_memory_profile", _resolve)
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)

    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="pnl",
            agent_id="trader",
            include_grounding=True,
            include_retrieval_debug=True,
        )
    )

    assert captured["project_filter"] == "alpha"
    assert captured["topic_filter"] == "strategy/live"
    assert captured["auto_escalate"] is True
    assert captured["query_expansion"] is True
    assert response["agent_id"] == "trader"
    assert response["retrieval_mode"] == "deep"
    assert response["grounding"]["strict_numeric_copy"] is True
    assert response["degraded"] is False


@pytest.mark.asyncio
async def test_context_pack_endpoint_returns_grounded_payload(monkeypatch: pytest.MonkeyPatch):
    async def _search(_: Any):
        return {
            "results": [
                {
                    "project": "alpha",
                    "file": "notes/a.md",
                    "summary": "Win rate reached 62.5%",
                    "score": 0.88,
                    "source": "qdrant",
                    "topic_path": "trading/metrics",
                    "created_at": "2026-03-02T10:00:00Z",
                }
            ],
            "grounding": {
                "strict_numeric_copy": True,
                "facts": [
                    {
                        "id": "fact_1",
                        "fact": "Win rate reached 62.5%",
                        "snippet": "Win rate reached 62.5%",
                        "score": 0.88,
                        "source": {
                            "project": "alpha",
                            "file": "notes/a.md",
                            "source": "qdrant",
                            "topic_path": "trading/metrics",
                            "timestamp": "2026-03-02T10:00:00Z",
                        },
                        "numeric_values": ["62.5%"],
                    }
                ],
                "numeric_facts": [
                    {
                        "value": "62.5%",
                        "snippet": "Win rate reached 62.5%",
                        "source": {
                            "project": "alpha",
                            "file": "notes/a.md",
                            "source": "qdrant",
                            "topic_path": "trading/metrics",
                            "timestamp": "2026-03-02T10:00:00Z",
                        },
                        "verbatim": True,
                    }
                ],
            },
            "warnings": [],
            "retrieval_mode": "balanced",
            "agent_id": "default",
        }

    monkeypatch.setattr(orchestrator, "search_memory", _search)
    response = await orchestrator.get_memory_context_pack(
        orchestrator.ContextPackRequest(query="win rate", limit=5, max_facts=5)
    )
    pack = response["context_pack"]
    assert pack["factualOnly"] is True
    assert pack["strictNumericCopy"] is True
    assert pack["numericFacts"][0]["value"] == "62.5%"
    assert pack["citations"][0]["file"] == "notes/a.md"


@pytest.mark.asyncio
async def test_agent_memory_profile_crud(monkeypatch: pytest.MonkeyPatch, tmp_path: Path):
    profile_path = tmp_path / "agent_memory_profiles.json"
    monkeypatch.setattr(orchestrator, "AGENT_MEMORY_PROFILE_PATH", profile_path)
    async with orchestrator.agent_memory_profile_lock:
        orchestrator.agent_memory_profiles.clear()
        orchestrator.agent_memory_profiles["default"] = orchestrator._default_agent_memory_profile()

    upsert = await orchestrator.upsert_agent_memory_profile(
        "agent-x",
        orchestrator.AgentMemoryProfileUpdate(
            retrieval_mode="fast",
            sources=["qdrant", "mindsdb"],
            default_project="alpha",
            auto_escalate=False,
        ),
    )
    assert upsert["ok"] is True
    assert upsert["profile"]["default_project"] == "alpha"
    assert upsert["profile"]["auto_escalate"] is False

    listing = await orchestrator.list_agent_memory_profiles()
    assert "agent-x" in listing["profiles"]

    fetched = await orchestrator.get_agent_memory_profile("agent-x")
    assert fetched["exists"] is True
    assert fetched["profile"]["retrieval_mode"] == "fast"

    deleted = await orchestrator.delete_agent_memory_profile("agent-x")
    assert deleted["ok"] is True
    assert deleted["deleted"] == "agent-x"

    missing = await orchestrator.get_agent_memory_profile("agent-x")
    assert missing["exists"] is False


@pytest.mark.asyncio
async def test_recall_eval_reports_metrics_and_gate(monkeypatch: pytest.MonkeyPatch):
    async def _search(payload: Any):
        if payload.query == "case one":
            return {
                "results": [
                    {
                        "project": "alpha",
                        "file": "docs/a.md",
                        "summary": "Revenue reached $1200 in March",
                        "score": 0.92,
                        "source": "mongo_raw",
                    }
                ],
                "grounding": {
                    "strict_numeric_copy": True,
                    "facts": [],
                    "numeric_facts": [{"value": "$1200", "verbatim": True}],
                },
                "warnings": [],
                "retrieval_mode": "balanced",
                "agent_id": "default",
            }
        return {
            "results": [
                {
                    "project": "alpha",
                    "file": "docs/other.md",
                    "summary": "No relevant hit",
                    "score": 0.31,
                    "source": "mongo_raw",
                }
            ],
            "grounding": {"strict_numeric_copy": True, "facts": [], "numeric_facts": []},
            "warnings": [],
            "retrieval_mode": "balanced",
            "agent_id": "default",
        }

    monkeypatch.setattr(orchestrator, "search_memory", _search)

    result = await orchestrator.evaluate_memory_recall(
        orchestrator.RecallEvalRequest(
            cases=[
                orchestrator.RecallEvalCase(
                    id="c1",
                    query="case one",
                    expected_files=["docs/a.md"],
                    expected_numeric=["$1200"],
                ),
                orchestrator.RecallEvalCase(
                    id="c2",
                    query="case two",
                    expected_files=["docs/missing.md"],
                ),
            ],
            k=3,
            gate_min_recall_at_k=0.4,
            gate_min_mrr=0.4,
            gate_min_numeric_exactness=0.8,
        )
    )

    assert result["passed"] is True
    assert result["metrics"]["casesEvaluated"] == 2
    assert result["metrics"]["recallAtK"] == 0.5
    assert result["metrics"]["mrr"] == 0.5
    assert result["metrics"]["numericExactness"] == 1.0


@pytest.mark.asyncio
async def test_get_recall_metrics_returns_alerts(monkeypatch: pytest.MonkeyPatch):
    async def _snapshot():
        return {
            "updatedAt": "2026-03-04T00:00:00Z",
            "requests": 120,
            "noHit": 42,
            "lowConfidence": 35,
            "staleHit": 14,
            "noHitRate": 0.35,
            "lowConfidenceRate": 0.29,
            "staleHitRate": 0.12,
            "bySource": {},
            "recent": [],
        }

    def _alerts(_: dict[str, Any]):
        return [{"code": "recall_no_hit_rate_high", "severity": "warn"}]

    monkeypatch.setattr(orchestrator, "_recall_quality_snapshot", _snapshot)
    monkeypatch.setattr(orchestrator, "_build_recall_quality_alerts", _alerts)
    payload = await orchestrator.get_recall_metrics()
    assert payload["alerts"]["count"] == 1
    assert payload["alerts"]["active"][0]["code"] == "recall_no_hit_rate_high"


@pytest.mark.asyncio
async def test_recall_tuning_endpoint_uses_monitor_samples(monkeypatch: pytest.MonkeyPatch):
    async def _samples(_lookback: float, max_samples: int):
        assert max_samples == 50
        return [
            {
                "timestamp": "2026-03-04T00:00:00Z",
                "noHitRate": 0.32,
                "lowConfidenceRate": 0.28,
                "staleHitRate": 0.12,
                "maxSourceErrorRate": 0.22,
                "lettaP95Ms": 21000.0,
                "lettaP99Ms": 30000.0,
                "lettaTimeoutRate": 0.03,
            },
            {
                "timestamp": "2026-03-04T00:15:00Z",
                "noHitRate": 0.36,
                "lowConfidenceRate": 0.33,
                "staleHitRate": 0.15,
                "maxSourceErrorRate": 0.26,
                "lettaP95Ms": 23000.0,
                "lettaP99Ms": 33000.0,
                "lettaTimeoutRate": 0.04,
            },
        ]

    async def _snapshot(limit: int):
        return {"state": {"runs": 2}, "history": [], "historySize": limit, "path": "/tmp/recall_monitor.ndjson"}

    monkeypatch.setattr(orchestrator, "_recall_monitor_samples_for_window", _samples)
    monkeypatch.setattr(orchestrator, "_recall_monitor_snapshot", _snapshot)
    payload = await orchestrator.get_recall_tuning(lookback_hours=24, min_samples=2, max_samples=50)
    assert payload["window"]["samples"] == 2
    assert payload["recommended"]["recall"]["noHitRate"] >= 0.36
    assert payload["recommended"]["retrieval"]["lettaP95Ms"] >= 23000.0
    assert payload["monitor"]["state"]["runs"] == 2


@pytest.mark.asyncio
async def test_get_saved_recall_eval_cases_reads_file(monkeypatch: pytest.MonkeyPatch, tmp_path: Path):
    case_path = tmp_path / "recall_eval_cases.json"
    case_path.write_text(
        json.dumps(
            {
                "version": 1,
                "updatedAt": "2026-03-04T00:00:00Z",
                "k": 4,
                "gate": {"minRecallAtK": 0.6, "minMrr": 0.5, "minNumericExactness": 0.8},
                "cases": [{"id": "c1", "query": "health status", "expected_substrings": ["health"]}],
            }
        ),
        encoding="utf-8",
    )
    monkeypatch.setattr(orchestrator, "RECALL_EVAL_CASES_PATH", case_path)
    payload = await orchestrator.get_saved_recall_eval_cases()
    assert payload["count"] == 1
    assert payload["k"] == 4
    assert payload["cases"][0]["id"] == "c1"


@pytest.mark.asyncio
async def test_evaluate_saved_recall_cases_uses_defaults(monkeypatch: pytest.MonkeyPatch, tmp_path: Path):
    case_path = tmp_path / "recall_eval_cases.json"
    case_path.write_text(
        json.dumps(
            {
                "version": 1,
                "updatedAt": "2026-03-04T00:00:00Z",
                "k": 3,
                "gate": {"minRecallAtK": 0.5, "minMrr": 0.4, "minNumericExactness": 0.9},
                "cases": [{"id": "c1", "query": "health status", "expected_substrings": ["health"]}],
            }
        ),
        encoding="utf-8",
    )
    monkeypatch.setattr(orchestrator, "RECALL_EVAL_CASES_PATH", case_path)
    captured: dict[str, Any] = {}

    async def _evaluate(payload: Any):
        captured["payload"] = payload
        return {"ok": True, "passed": True, "metrics": {}, "gate": {}, "cases": []}

    monkeypatch.setattr(orchestrator, "evaluate_memory_recall", _evaluate)
    result = await orchestrator.evaluate_saved_recall_cases(orchestrator.RecallEvalSavedRequest())
    assert result["ok"] is True
    assert result["savedCaseSet"]["count"] == 1
    assert captured["payload"].k == 3
    assert captured["payload"].gate_min_recall_at_k == 0.5


def test_list_topics_snapshot_sorts_and_filters(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(
        orchestrator,
        "topic_tree",
        {
            "alpha": {
                "count": 6,
                "children": {
                    "channels": {
                        "count": 6,
                        "children": {
                            "telegram": {"count": 4, "children": {}},
                            "slack": {"count": 2, "children": {}},
                        },
                    }
                },
            },
            "beta": {
                "count": 3,
                "children": {
                    "channels": {
                        "count": 3,
                        "children": {
                            "telegram": {"count": 3, "children": {}},
                        },
                    }
                },
            },
        },
    )

    result = orchestrator._list_topics_snapshot(prefix="channels/telegram", limit=10, min_count=3, depth=8)
    assert result["total"] == 2
    assert [item["project"] for item in result["topics"]] == ["alpha", "beta"]
    assert [item["path"] for item in result["topics"]] == ["channels/telegram", "channels/telegram"]
    assert [item["count"] for item in result["topics"]] == [4, 3]


@pytest.mark.asyncio
async def test_tool_topics_list_project_scope(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(
        orchestrator,
        "topic_tree",
        {
            "alpha": {
                "count": 5,
                "children": {
                    "root": {
                        "count": 5,
                        "children": {
                            "docs": {"count": 2, "children": {}},
                            "code": {"count": 3, "children": {}},
                        },
                    }
                },
            }
        },
    )

    payload = orchestrator.TopicsListRequest(project="alpha", prefix="root", min_count=2, limit=10, depth=6)
    result = await orchestrator.tool_topics_list(payload)
    assert result["project"] == "alpha"
    assert result["total"] == 3
    assert result["topics"][0]["path"] == "root"
    assert result["topics"][0]["count"] == 5


@pytest.mark.asyncio
async def test_openclaw_surface_blocks_secret_like_remember_content(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "MESSAGING_OPENCLAW_STRICT_SECURITY", True)
    parsed = {
        "action": "remember",
        "content": "api_key=sk-1234567890abcdefghijklmno",
        "directives": {},
        "raw": "remember api_key=sk-1234567890abcdefghijklmno",
    }
    with pytest.raises(orchestrator.HTTPException) as exc:
        await orchestrator._execute_messaging_command(
            parsed,
            channel="openclaw",
            source_id="session-1",
            default_project="messaging",
            topic_root="channels/openclaw",
        )
    assert exc.value.status_code == 422


@pytest.mark.asyncio
async def test_openclaw_surface_redacts_secret_like_recall_output(monkeypatch: pytest.MonkeyPatch):
    async def _search(_: Any):
        return {
            "results": [
                {
                    "project": "messaging",
                    "file": "channels/openclaw/session-1/msg_1.md",
                    "summary": "token=supersecret123456789",
                    "source": "memory_bank",
                }
            ],
            "warnings": [],
        }

    monkeypatch.setattr(orchestrator, "MESSAGING_OPENCLAW_STRICT_SECURITY", True)
    monkeypatch.setattr(orchestrator, "search_memory", _search)
    parsed = {
        "action": "recall",
        "content": "status",
        "directives": {},
        "raw": "recall status",
    }
    result = await orchestrator._execute_messaging_command(
        parsed,
        channel="zeroclaw",
        source_id="session-1",
        default_project="messaging",
        topic_root="channels/zeroclaw",
    )
    rendered = json.dumps(result)
    assert "supersecret123456789" not in rendered
    assert "[REDACTED]" in rendered


@pytest.mark.asyncio
async def test_messaging_ironclaw_endpoint_disabled_by_default(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "IRONCLAW_INTEGRATION_ENABLED", False)
    payload = orchestrator.MessagingCommandIn(
        channel="ironclaw",
        source_id="wallet-1",
        text="@ContextLattice status",
    )
    with pytest.raises(orchestrator.HTTPException) as exc:
        await orchestrator.messaging_ironclaw(payload)
    assert exc.value.status_code == 503


@pytest.mark.asyncio
async def test_messaging_ironclaw_endpoint_bridges_to_command(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, Any] = {}

    async def _messaging_command(payload: Any):
        captured["payload"] = payload
        return {"ok": True}

    monkeypatch.setattr(orchestrator, "IRONCLAW_INTEGRATION_ENABLED", True)
    monkeypatch.setattr(orchestrator, "IRONCLAW_DEFAULT_PROJECT", "web3")
    monkeypatch.setattr(orchestrator, "messaging_command", _messaging_command)
    payload = orchestrator.MessagingCommandIn(
        channel="",
        source_id="wallet-1",
        text="@ContextLattice status",
    )
    result = await orchestrator.messaging_ironclaw(payload)
    bridged = captured["payload"]
    assert result["ok"] is True
    assert bridged.channel == "ironclaw"
    assert bridged.project == "web3"


@pytest.mark.asyncio
async def test_messaging_task_create_remember_enqueues_task(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, Any] = {}

    async def _create_task_record(title, project, agent, priority, payload, run_after=None, max_attempts=None):
        captured.update(
            {
                "title": title,
                "project": project,
                "agent": agent,
                "priority": priority,
                "payload": payload,
                "run_after": run_after,
                "max_attempts": max_attempts,
            }
        )
        return {
            "id": "task-123",
            "status": "queued",
            "action_type": "memory_write",
            "max_attempts": max_attempts or 4,
        }

    monkeypatch.setattr(orchestrator, "create_task_record", _create_task_record)
    parsed = {
        "action": "task",
        "content": "create remember deployment complete",
        "directives": {"priority": "3", "max_attempts": "6"},
        "raw": "task create remember deployment complete",
    }
    result = await orchestrator._execute_messaging_command(
        parsed,
        channel="custom",
        source_id="chat-1",
        default_project="alpha",
        topic_root="channels/custom",
    )
    assert result["ok"] is True
    assert result["subcommand"] == "create"
    assert captured["priority"] == 3
    assert captured["max_attempts"] == 6
    assert captured["payload"]["action"] == "memory_write"
    assert "task-123" in result["response_text"]


@pytest.mark.asyncio
async def test_messaging_task_status_returns_task_and_events(monkeypatch: pytest.MonkeyPatch):
    async def _get_task_record(task_id: str):
        assert task_id == "task-1"
        return {
            "id": task_id,
            "status": "running",
            "attempts": 1,
            "max_attempts": 4,
            "project": "alpha",
            "action_type": "memory_search",
        }

    async def _get_task_events(task_id: str):
        assert task_id == "task-1"
        return [{"id": 1, "status": "running"}]

    monkeypatch.setattr(orchestrator, "get_task_record", _get_task_record)
    monkeypatch.setattr(orchestrator, "get_task_events", _get_task_events)
    parsed = {
        "action": "task",
        "content": "status task-1",
        "directives": {},
        "raw": "task status task-1",
    }
    result = await orchestrator._execute_messaging_command(
        parsed,
        channel="custom",
        source_id="chat-1",
        default_project="alpha",
        topic_root="channels/custom",
    )
    assert result["ok"] is True
    assert result["subcommand"] == "status"
    assert result["result"]["task"]["id"] == "task-1"
    assert result["result"]["events"][0]["id"] == 1


@pytest.mark.asyncio
async def test_messaging_task_replay_calls_replay_task_record(monkeypatch: pytest.MonkeyPatch):
    called: dict[str, Any] = {}

    async def _replay(task_id: str, *, actor: str | None = None, note: str | None = None, reset_attempts: bool = True):
        called.update(
            {
                "task_id": task_id,
                "actor": actor,
                "note": note,
                "reset_attempts": reset_attempts,
            }
        )
        return {"id": task_id, "status": "queued", "attempts": 0, "max_attempts": 4}

    monkeypatch.setattr(orchestrator, "replay_task_record", _replay)
    parsed = {
        "action": "task",
        "content": "replay task-9",
        "directives": {},
        "raw": "task replay task-9",
    }
    result = await orchestrator._execute_messaging_command(
        parsed,
        channel="custom",
        source_id="chat-3",
        default_project="alpha",
        topic_root="channels/custom",
    )
    assert result["ok"] is True
    assert result["subcommand"] == "replay"
    assert called["task_id"] == "task-9"
    assert called["actor"] == "chat-3"
    assert called["reset_attempts"] is True


@pytest.mark.asyncio
async def test_get_task_runtime_snapshot_reports_counts(monkeypatch: pytest.MonkeyPatch, tmp_path: Path):
    db_path = tmp_path / "agent_tasks.db"
    monkeypatch.setattr(orchestrator, "TASK_DB_PATH", db_path)
    monkeypatch.setattr(orchestrator, "task_db_ready", False)
    await orchestrator.ensure_task_db()

    old_ts = "2000-01-01T00:00:00Z"
    future_ts = "2999-01-01T00:00:00Z"

    def _seed(conn):
        rows = [
            ("task-q", "queued", 0, 0, old_ts, 0, 3),
            ("task-a", "approved", 1, 1, old_ts, 0, 3),
            ("task-blocked", "approved", 1, 0, old_ts, 0, 3),
            ("task-future", "queued", 0, 0, future_ts, 0, 3),
            ("task-running", "running", 0, 0, old_ts, 1, 3),
            ("task-failed", "failed", 0, 0, old_ts, 3, 3),
        ]
        for task_id, status, approval_required, approved, run_after, attempts, max_attempts in rows:
            conn.execute(
                """
                INSERT INTO tasks (
                    id, title, status, project, agent, priority, payload, run_after, attempts, max_attempts,
                    lease_expires_at, claimed_by, last_error, result, completed_at, created_at, updated_at,
                    approval_required, approved, risk_level, action_type
                )
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    task_id,
                    f"title-{task_id}",
                    status,
                    "alpha",
                    None,
                    0,
                    "{}",
                    run_after,
                    attempts,
                    max_attempts,
                    None,
                    None,
                    None,
                    None,
                    None,
                    old_ts,
                    old_ts,
                    approval_required,
                    approved,
                    None,
                    "memory_write",
                ),
            )
        conn.commit()

    await orchestrator._task_db_exec(_seed)
    snapshot = await orchestrator.get_task_runtime_snapshot()
    assert snapshot["queueReady"] == 2
    assert snapshot["running"] == 1
    assert snapshot["deadletter"] == 1
    assert snapshot["byStatus"]["queued"] == 2
    assert snapshot["byStatus"]["approved"] == 2


@pytest.mark.asyncio
async def test_claim_next_task_respects_agent_affinity(monkeypatch: pytest.MonkeyPatch, tmp_path: Path):
    db_path = tmp_path / "agent_tasks.db"
    monkeypatch.setattr(orchestrator, "TASK_DB_PATH", db_path)
    monkeypatch.setattr(orchestrator, "task_db_ready", False)
    await orchestrator.ensure_task_db()

    old_ts = "2000-01-01T00:00:00Z"

    def _seed(conn):
        rows = [
            ("task-external", "codex-subagent", 9),
            ("task-internal", "internal", 8),
            ("task-unassigned", None, 1),
        ]
        for task_id, agent, priority in rows:
            conn.execute(
                """
                INSERT INTO tasks (
                    id, title, status, project, agent, priority, payload, run_after, attempts, max_attempts,
                    lease_expires_at, claimed_by, last_error, result, completed_at, created_at, updated_at,
                    approval_required, approved, risk_level, action_type
                )
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    task_id,
                    f"title-{task_id}",
                    "queued",
                    "alpha",
                    agent,
                    priority,
                    "{}",
                    old_ts,
                    0,
                    3,
                    None,
                    None,
                    None,
                    None,
                    None,
                    old_ts,
                    old_ts,
                    0,
                    0,
                    None,
                    "messaging_command",
                ),
            )
        conn.commit()

    await orchestrator._task_db_exec(_seed)
    internal_claim = await orchestrator.claim_next_task("internal-worker-1")
    assert internal_claim is not None
    assert internal_claim["id"] == "task-internal"
    external_claim = await orchestrator.claim_next_task("internal-worker-1")
    assert external_claim is not None
    assert external_claim["id"] == "task-unassigned"
    no_more_internal = await orchestrator.claim_next_task("internal-worker-1")
    assert no_more_internal is None
    codex_claim = await orchestrator.claim_next_task("codex-subagent")
    assert codex_claim is not None
    assert codex_claim["id"] == "task-external"


@pytest.mark.asyncio
async def test_list_task_records_filters_by_agent(monkeypatch: pytest.MonkeyPatch, tmp_path: Path):
    db_path = tmp_path / "agent_tasks.db"
    monkeypatch.setattr(orchestrator, "TASK_DB_PATH", db_path)
    monkeypatch.setattr(orchestrator, "task_db_ready", False)
    await orchestrator.ensure_task_db()

    old_ts = "2000-01-01T00:00:00Z"

    def _seed(conn):
        rows = [
            ("task-a", "codex-subagent"),
            ("task-b", ""),
            ("task-c", None),
        ]
        for task_id, agent in rows:
            conn.execute(
                """
                INSERT INTO tasks (
                    id, title, status, project, agent, priority, payload, run_after, attempts, max_attempts,
                    lease_expires_at, claimed_by, last_error, result, completed_at, created_at, updated_at,
                    approval_required, approved, risk_level, action_type
                )
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    task_id,
                    f"title-{task_id}",
                    "queued",
                    "alpha",
                    agent,
                    0,
                    "{}",
                    old_ts,
                    0,
                    3,
                    None,
                    None,
                    None,
                    None,
                    None,
                    old_ts,
                    old_ts,
                    0,
                    0,
                    None,
                    "memory_write",
                ),
            )
        conn.commit()

    await orchestrator._task_db_exec(_seed)
    codex_tasks = await orchestrator.list_task_records(project="alpha", agent="codex-subagent", limit=10)
    assert [item["id"] for item in codex_tasks] == ["task-a"]
    unassigned_tasks = await orchestrator.list_task_records(project="alpha", agent="unassigned", limit=10)
    assert [item["id"] for item in unassigned_tasks] == ["task-b", "task-c"]


@pytest.mark.asyncio
async def test_fanout_summary_returns_stale_cache_and_schedules_refresh(
    monkeypatch: pytest.MonkeyPatch,
):
    scheduled: dict[str, bool] = {"called": False}

    def _schedule():
        scheduled["called"] = True

    monkeypatch.setattr(orchestrator, "_schedule_fanout_summary_refresh", _schedule)
    orchestrator.fanout_summary_cache["by_status"] = {"pending": 2}
    orchestrator.fanout_summary_cache["by_target"] = {"qdrant": {"pending": 2}}
    orchestrator.fanout_summary_cache["updated_monotonic"] = time.monotonic() - 999
    orchestrator.fanout_summary_cache["updated_at"] = "2026-02-10T00:00:00Z"

    summary = await orchestrator.get_fanout_summary()
    assert summary["by_status"]["pending"] == 2
    assert scheduled["called"] is True


@pytest.mark.asyncio
async def test_run_fanout_outbox_gc_once_prunes_sqlite(monkeypatch: pytest.MonkeyPatch, tmp_path: Path):
    db_path = tmp_path / "agent_tasks.db"
    monkeypatch.setattr(orchestrator, "TASK_DB_PATH", db_path)
    monkeypatch.setattr(orchestrator, "task_db_ready", False)
    monkeypatch.setattr(orchestrator, "fanout_outbox_backend_active", "sqlite")
    monkeypatch.setattr(orchestrator, "FANOUT_OUTBOX_SUCCEEDED_RETENTION_HOURS", 24)
    monkeypatch.setattr(orchestrator, "FANOUT_OUTBOX_FAILED_RETENTION_HOURS", 168)
    monkeypatch.setattr(orchestrator, "FANOUT_OUTBOX_STALE_PENDING_HOURS", 24)
    monkeypatch.setattr(orchestrator, "FANOUT_OUTBOX_STALE_TARGETS", [orchestrator.FANOUT_TARGET_LETTA])
    monkeypatch.setattr(orchestrator, "FANOUT_OUTBOX_GC_VACUUM", False)
    monkeypatch.setattr(orchestrator, "FANOUT_OUTBOX_GC_TIMEOUT_SECS", 10.0)
    monkeypatch.setattr(orchestrator, "outbox_gc_last_vacuum_monotonic", 0.0)
    monkeypatch.setitem(
        orchestrator.outbox_health,
        "gc",
        {
            "lastRunAt": None,
            "lastDurationMs": None,
            "lastDeleted": 0,
            "lastError": None,
            "runs": 0,
            "vacuumedAt": None,
        },
    )
    await orchestrator.ensure_task_db()

    old_ts = "2000-01-01T00:00:00Z"
    fresh_ts = "2999-01-01T00:00:00Z"

    def _seed(conn):
        rows = [
            (
                "evt-1",
                orchestrator.FANOUT_TARGET_QDRANT,
                "alpha",
                "notes/a.md",
                "{}",
                "succeeded",
                old_ts,
                old_ts,
                old_ts,
                old_ts,
                "evt-1:qdrant",
            ),
            (
                "evt-2",
                orchestrator.FANOUT_TARGET_MINDSDB,
                "alpha",
                "notes/b.md",
                "{}",
                "failed",
                old_ts,
                old_ts,
                old_ts,
                old_ts,
                "evt-2:mindsdb",
            ),
            (
                "evt-3",
                orchestrator.FANOUT_TARGET_LETTA,
                "alpha",
                "notes/c.md",
                "{}",
                "retrying",
                old_ts,
                old_ts,
                old_ts,
                None,
                "evt-3:letta",
            ),
            (
                "evt-4",
                orchestrator.FANOUT_TARGET_QDRANT,
                "alpha",
                "notes/d.md",
                "{}",
                "succeeded",
                fresh_ts,
                fresh_ts,
                fresh_ts,
                fresh_ts,
                "evt-4:qdrant",
            ),
        ]
        for event_id, target, project, file_name, payload, status, next_at, created_at, updated_at, completed_at, dedupe_key in rows:
            conn.execute(
                """
                INSERT INTO fanout_outbox (
                    event_id, target, project, file, summary, payload, topic_path, topic_tags,
                    status, attempts, max_attempts, next_attempt_at, last_attempt_at, completed_at,
                    last_error, created_at, updated_at, dedupe_key
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    event_id,
                    target,
                    project,
                    file_name,
                    "",
                    payload,
                    "root",
                    "[]",
                    status,
                    0,
                    10,
                    next_at,
                    old_ts,
                    completed_at,
                    None,
                    created_at,
                    updated_at,
                    dedupe_key,
                ),
            )
        conn.commit()

    await orchestrator._task_db_exec(_seed)
    result = await orchestrator.run_fanout_outbox_gc_once()
    assert result["backend"] == "sqlite"
    assert result["deleted_total"] == 3
    assert result["deleted"]["succeeded"] == 1
    assert result["deleted"]["failed"] == 1
    assert result["deleted"]["stale_pending_targets"] == 1
    assert result["after_total"] == 1
    assert orchestrator.outbox_health["gc"]["lastDeleted"] == 3
    assert orchestrator.outbox_health["gc"]["lastError"] is None


@pytest.mark.asyncio
async def test_enqueue_fanout_outbox_coalesces_recent_sqlite_rows(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
):
    db_path = tmp_path / "agent_tasks.db"
    monkeypatch.setattr(orchestrator, "TASK_DB_PATH", db_path)
    monkeypatch.setattr(orchestrator, "task_db_ready", False)
    monkeypatch.setattr(orchestrator, "fanout_outbox_backend_active", "sqlite")
    monkeypatch.setattr(orchestrator, "FANOUT_COALESCE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "FANOUT_COALESCE_WINDOW_SECS", 30.0)
    monkeypatch.setattr(orchestrator, "FANOUT_COALESCE_TARGETS", [orchestrator.FANOUT_TARGET_QDRANT])
    orchestrator.fanout_coalesce_total = 0
    orchestrator.fanout_coalesce_by_target.clear()
    await orchestrator.ensure_task_db()

    payload1 = {
        "event_id": "evt-1",
        "project": "alpha",
        "file": "notes/a.md",
        "summary": "first summary",
        "payload": {"projectName": "alpha", "fileName": "notes/a.md"},
        "topic_path": "notes",
        "topic_tags": ["notes"],
    }
    payload2 = {
        "event_id": "evt-2",
        "project": "alpha",
        "file": "notes/a.md",
        "summary": "latest summary",
        "payload": {"projectName": "alpha", "fileName": "notes/a.md"},
        "topic_path": "notes",
        "topic_tags": ["notes"],
    }

    first = await orchestrator.enqueue_fanout_outbox(payload1, [orchestrator.FANOUT_TARGET_QDRANT])
    second = await orchestrator.enqueue_fanout_outbox(payload2, [orchestrator.FANOUT_TARGET_QDRANT])
    assert first["inserted"] == 1
    assert second["inserted"] == 0
    assert second["coalesced"] == 1
    assert orchestrator.fanout_coalesce_total >= 1

    jobs = await orchestrator.list_fanout_jobs(["pending", "retrying", "running"], limit=10)
    assert len(jobs) == 1
    assert jobs[0]["summary"] == "latest summary"


@pytest.mark.asyncio
async def test_enqueue_fanout_outbox_coalesces_stale_for_configured_target(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
):
    db_path = tmp_path / "agent_tasks.db"
    monkeypatch.setattr(orchestrator, "TASK_DB_PATH", db_path)
    monkeypatch.setattr(orchestrator, "task_db_ready", False)
    monkeypatch.setattr(orchestrator, "fanout_outbox_backend_active", "sqlite")
    monkeypatch.setattr(orchestrator, "FANOUT_COALESCE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "FANOUT_COALESCE_WINDOW_SECS", 1.0)
    monkeypatch.setattr(orchestrator, "FANOUT_COALESCE_TARGETS", [orchestrator.FANOUT_TARGET_LETTA])
    monkeypatch.setattr(orchestrator, "FANOUT_COALESCE_STALE_TARGETS", [orchestrator.FANOUT_TARGET_LETTA])
    await orchestrator.ensure_task_db()

    old_ts = "2000-01-01T00:00:00Z"

    def _seed(conn):
        conn.execute(
            """
            INSERT INTO fanout_outbox (
                event_id, target, project, file, summary, payload, topic_path, topic_tags,
                status, attempts, max_attempts, next_attempt_at, last_attempt_at, completed_at,
                last_error, created_at, updated_at, dedupe_key
            ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                "evt-stale",
                orchestrator.FANOUT_TARGET_LETTA,
                "alpha",
                "index__exits.json",
                "old summary",
                "{}",
                "root",
                "[]",
                "pending",
                0,
                10,
                old_ts,
                old_ts,
                None,
                None,
                old_ts,
                old_ts,
                "evt-stale:letta",
            ),
        )
        conn.commit()

    await orchestrator._task_db_exec(_seed)
    payload = {
        "event_id": "evt-new",
        "project": "alpha",
        "file": "index__exits.json",
        "summary": "new summary",
        "payload": {"projectName": "alpha", "fileName": "index__exits.json"},
        "topic_path": "root",
        "topic_tags": [],
    }
    result = await orchestrator.enqueue_fanout_outbox(payload, [orchestrator.FANOUT_TARGET_LETTA])
    assert result["inserted"] == 0
    assert result["coalesced"] == 1
    jobs = await orchestrator.list_fanout_jobs(["pending", "retrying", "running"], limit=10)
    assert len(jobs) == 1
    assert jobs[0]["summary"] == "new summary"


@pytest.mark.asyncio
async def test_federated_search_staged_fetch_skips_slow_sources(
    monkeypatch: pytest.MonkeyPatch,
):
    slow_calls = {"letta": 0, "memory_bank": 0}

    async def _qdrant(*args, **kwargs):
        return [
            {
                "project": "alpha",
                "file": "notes/a.txt",
                "summary": "high confidence answer",
                "score": 0.95,
                "source": "qdrant",
            }
        ]

    async def _mongo(*args, **kwargs):
        return []

    async def _mindsdb(*args, **kwargs):
        return []

    async def _topic_rollups(*args, **kwargs):
        return []

    async def _letta(*args, **kwargs):
        slow_calls["letta"] += 1
        return []

    async def _memory_bank(*args, **kwargs):
        slow_calls["memory_bank"] += 1
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _mongo)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _mindsdb)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _topic_rollups)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _memory_bank)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_FAST_SOURCES",
        ["qdrant", "mongo_raw", "mindsdb"],
    )
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_SLOW_SOURCES",
        ["letta", "memory_bank"],
    )
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_RESULTS", 1)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_TOP_SCORE", 0.8)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_DIVERSITY", 1)

    results, debug, _ = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=["qdrant", "mongo_raw", "mindsdb", "topic_rollups", "letta", "memory_bank"],
        rerank_with_learning=False,
    )
    assert results
    assert slow_calls == {"letta": 0, "memory_bank": 0}
    assert debug["staged_fetch"]["slow_sources_skipped"] == ["letta", "memory_bank"]


def test_mongo_timestamp_and_outstanding_helpers():
    now = datetime(2026, 2, 10, 12, 0, 0)
    rendered = orchestrator._mongo_timestamp_iso(now)
    assert rendered.startswith("2026-02-10T12:00:00")
    summary = {"by_status": {"pending": 2, "retrying": 3, "running": 1}}
    assert orchestrator._fanout_outstanding(summary) == 6


@pytest.mark.asyncio
async def test_letta_admission_drops_low_value_when_backlog_high(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "LETTA_ADMISSION_ENABLED", True)
    monkeypatch.setattr(orchestrator, "LETTA_ADMISSION_BACKLOG_SOFT_LIMIT", 5)
    monkeypatch.setattr(orchestrator, "LETTA_ADMISSION_BACKLOG_HARD_LIMIT", 20)
    monkeypatch.setattr(orchestrator, "LETTA_ADMISSION_LOW_VALUE_MIN_SUMMARY_CHARS", 80)
    monkeypatch.setattr(orchestrator, "LETTA_EXCLUDED_FILE_PATTERNS", [])
    monkeypatch.setattr(orchestrator, "LETTA_EXCLUDED_TOPIC_PREFIXES", [])
    orchestrator.fanout_summary_cache["by_status"] = {"pending": 5}
    orchestrator.fanout_summary_cache["by_target"] = {"letta": {"pending": 6}}
    orchestrator.fanout_summary_cache["updated_monotonic"] = time.monotonic()

    admit_low, reason_low, backlog_low = await orchestrator._letta_admission_should_enqueue(
        "telemetry/queue__latest.json",
        "telemetry",
        "queue sample",
        "memory_write",
    )
    assert admit_low is False
    assert reason_low == "soft_backlog_low_value"
    assert backlog_low == 6

    admit_high, reason_high, _ = await orchestrator._letta_admission_should_enqueue(
        "decisions/2026-02-16-architecture.md",
        "decisions",
        "This is a longer architectural note that should not be treated as low value.",
        "memory_write",
    )
    assert admit_high is True
    assert reason_high is None


@pytest.mark.asyncio
async def test_letta_admission_drops_excluded_patterns_without_backlog(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "LETTA_ADMISSION_ENABLED", True)
    monkeypatch.setattr(orchestrator, "LETTA_EXCLUDED_FILE_PATTERNS", ["index__*.json"])
    monkeypatch.setattr(orchestrator, "LETTA_EXCLUDED_TOPIC_PREFIXES", [])
    orchestrator.fanout_summary_cache["by_status"] = {"pending": 0}
    orchestrator.fanout_summary_cache["by_target"] = {"letta": {"pending": 0}}
    orchestrator.fanout_summary_cache["updated_monotonic"] = time.monotonic()

    admit, reason, backlog = await orchestrator._letta_admission_should_enqueue(
        "index__exits.json",
        "root",
        "telemetry snapshot",
        "memory_write",
    )
    assert admit is False
    assert reason == "excluded_file_pattern"
    assert backlog == 0


def test_low_value_classifier_helpers():
    assert orchestrator._is_low_value_memory_record(
        "telemetry/queue__latest.json",
        "telemetry",
        "queue depth",
        include_short_summary=True,
    )
    assert orchestrator._is_low_value_memory_record(
        "notes/flow.md",
        "signals/live",
        "signal update",
        include_short_summary=False,
    )
    assert not orchestrator._is_low_value_memory_record(
        "decisions/rfc.md",
        "decisions",
        "Long-form decision artifact",
        include_short_summary=False,
    )


@pytest.mark.asyncio
async def test_prune_letta_low_value_outbox_sqlite(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
):
    db_path = tmp_path / "agent_tasks.db"
    monkeypatch.setattr(orchestrator, "TASK_DB_PATH", db_path)
    monkeypatch.setattr(orchestrator, "task_db_ready", False)
    monkeypatch.setattr(orchestrator, "fanout_outbox_backend_active", "sqlite")
    monkeypatch.setattr(orchestrator, "LETTA_EXCLUDED_FILE_PATTERNS", ["index__*.json"])
    monkeypatch.setattr(orchestrator, "LETTA_EXCLUDED_TOPIC_PREFIXES", [])
    await orchestrator.ensure_task_db()

    now_ts = "2026-03-06T00:00:00Z"

    def _seed(conn):
        rows = [
            ("evt-1", "index__exits.json", "root", "pending", "evt-1:letta"),
            ("evt-2", "decisions/rfc.md", "decisions", "pending", "evt-2:letta"),
        ]
        for event_id, file_name, topic_path, status, dedupe_key in rows:
            conn.execute(
                """
                INSERT INTO fanout_outbox (
                    event_id, target, project, file, summary, payload, topic_path, topic_tags,
                    status, attempts, max_attempts, next_attempt_at, last_attempt_at, completed_at,
                    last_error, created_at, updated_at, dedupe_key
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    event_id,
                    orchestrator.FANOUT_TARGET_LETTA,
                    "alpha",
                    file_name,
                    "summary",
                    "{}",
                    topic_path,
                    "[]",
                    status,
                    0,
                    10,
                    now_ts,
                    now_ts,
                    None,
                    None,
                    now_ts,
                    now_ts,
                    dedupe_key,
                ),
            )
        conn.commit()

    await orchestrator._task_db_exec(_seed)
    result = await orchestrator.prune_letta_low_value_outbox(
        statuses=["pending"],
        limit=100,
        dry_run=False,
    )
    assert result["backend"] == "sqlite"
    assert result["beforePending"] == 2
    assert result["matched"] == 1
    assert result["deleted"] == 1
    assert result["afterPending"] == 1


@pytest.mark.asyncio
async def test_run_sink_retention_once_collects_partial_errors(monkeypatch: pytest.MonkeyPatch):
    async def _qdrant():
        return {"enabled": True, "deleted": 2}

    async def _mongo():
        raise RuntimeError("mongo unavailable")

    async def _letta():
        return {"enabled": True, "deleted": 0}

    monkeypatch.setattr(orchestrator, "_run_qdrant_low_value_retention_once", _qdrant)
    monkeypatch.setattr(orchestrator, "_run_mongo_low_value_retention_once", _mongo)
    monkeypatch.setattr(orchestrator, "_run_letta_low_value_retention_once", _letta)
    orchestrator.sink_retention_state.update(
        {
            "lastRunAt": None,
            "lastDurationMs": None,
            "lastError": None,
            "runs": 0,
            "lastResult": {},
        }
    )

    result = await orchestrator.run_sink_retention_once()
    assert result["ok"] is False
    assert result["sinks"]["qdrant"]["deleted"] == 2
    assert "mongo_raw" in result["errors"]
    assert orchestrator.sink_retention_state["runs"] == 1


@pytest.mark.asyncio
async def test_should_skip_duplicate_memory_write(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "MEMORY_WRITE_DEDUP_ENABLED", True)
    monkeypatch.setattr(orchestrator, "MEMORY_WRITE_DEDUP_WINDOW_SECS", 120.0)
    monkeypatch.setattr(orchestrator, "MEMORY_WRITE_DEDUP_MAX_KEYS", 1000)
    orchestrator.memory_write_dedupe_seen.clear()
    key = orchestrator.build_memory_write_dedupe_key("alpha", "notes/a.md", "same payload")
    assert await orchestrator.should_skip_duplicate_memory_write(key, now_monotonic=100.0) is False
    assert await orchestrator.should_skip_duplicate_memory_write(key, now_monotonic=150.0) is True
    assert await orchestrator.should_skip_duplicate_memory_write(key, now_monotonic=400.0) is False


@pytest.mark.asyncio
async def test_read_project_file_allow_missing_returns_empty(monkeypatch: pytest.MonkeyPatch):
    async def _missing(*args, **kwargs):
        raise orchestrator.HTTPException(
            500,
            "memory_bank_read failed: NotFoundError: Resource not found: missing.json",
        )

    monkeypatch.setattr(orchestrator, "call_memory_tool", _missing)
    content = await orchestrator.read_project_file(
        "algotraderv2_rust",
        "missing.json",
        allow_missing=True,
    )
    assert content == ""


@pytest.mark.asyncio
async def test_read_project_file_bootstraps_missing_index(monkeypatch: pytest.MonkeyPatch):
    calls: list[str] = []

    async def _fake_tool(name: str, arguments: dict[str, object]):
        calls.append(name)
        if name == "memory_bank_read":
            raise orchestrator.HTTPException(
                500,
                "memory_bank_read failed: NotFoundError: Resource not found: index__arena_health.json",
            )
        if name == "memory_bank_write":
            assert arguments["projectName"] == "algotraderv2_rust"
            assert arguments["fileName"] == "index__arena_health.json"
            return {"isError": False, "content": [{"type": "text", "text": "ok"}]}
        raise AssertionError(f"unexpected tool call: {name}")

    monkeypatch.setattr(orchestrator, "call_memory_tool", _fake_tool)
    content = await orchestrator.read_project_file(
        "algotraderv2_rust",
        "index__arena_health.json",
        allow_missing=True,
        bootstrap_missing=True,
    )
    parsed = json.loads(content)
    assert parsed["bootstrap"] is True
    assert parsed["latest"] == "arena__health__latest.json"
    assert calls == ["memory_bank_read", "memory_bank_write"]


@pytest.mark.asyncio
async def test_read_project_file_timeout_serves_stale_cache(monkeypatch: pytest.MonkeyPatch):
    async def _timeout_remote(*args, **kwargs):
        raise asyncio.TimeoutError()

    refreshed: list[tuple[str, str]] = []

    async def _schedule_refresh(project: str, file_name: str):
        refreshed.append((project, file_name))

    monkeypatch.setattr(orchestrator, "MEMMCP_READ_FAIL_OPEN_ENABLED", True)
    monkeypatch.setattr(orchestrator, "MEMMCP_READ_CACHE_MAX_KEYS", 64)
    monkeypatch.setattr(orchestrator, "MEMMCP_READ_CACHE_FRESH_TTL_SECS", 0.0)
    monkeypatch.setattr(orchestrator, "MEMMCP_READ_CACHE_STALE_MAX_SECS", 3600.0)
    monkeypatch.setattr(orchestrator, "_read_project_file_remote", _timeout_remote)
    monkeypatch.setattr(orchestrator, "_schedule_memory_read_cache_refresh", _schedule_refresh)
    monkeypatch.setattr(orchestrator, "memory_read_cache_stale_fallbacks", 0)
    async with orchestrator.memory_read_cache_lock:
        orchestrator.memory_read_cache.clear()
    await orchestrator._memory_read_cache_set("alpha", "notes/a.md", "cached-content")

    content = await orchestrator.read_project_file("alpha", "notes/a.md")
    assert content == "cached-content"
    assert refreshed == [("alpha", "notes/a.md")]
    assert orchestrator.memory_read_cache_stale_fallbacks == 1


@pytest.mark.asyncio
async def test_read_project_file_timeout_without_cache_raises_504(monkeypatch: pytest.MonkeyPatch):
    async def _timeout_remote(*args, **kwargs):
        raise asyncio.TimeoutError()

    monkeypatch.setattr(orchestrator, "MEMMCP_READ_FAIL_OPEN_ENABLED", True)
    monkeypatch.setattr(orchestrator, "_read_project_file_remote", _timeout_remote)
    async with orchestrator.memory_read_cache_lock:
        orchestrator.memory_read_cache.clear()

    with pytest.raises(orchestrator.HTTPException) as exc:
        await orchestrator.read_project_file("alpha", "notes/a.md")
    assert exc.value.status_code == 504


@pytest.mark.asyncio
async def test_read_project_file_success_updates_cache(monkeypatch: pytest.MonkeyPatch):
    async def _remote(*args, **kwargs):
        return "live-content"

    monkeypatch.setattr(orchestrator, "MEMMCP_READ_FAIL_OPEN_ENABLED", True)
    monkeypatch.setattr(orchestrator, "MEMMCP_READ_CACHE_MAX_KEYS", 64)
    monkeypatch.setattr(orchestrator, "_read_project_file_remote", _remote)
    async with orchestrator.memory_read_cache_lock:
        orchestrator.memory_read_cache.clear()

    content = await orchestrator.read_project_file("alpha", "notes/a.md")
    cached = await orchestrator._memory_read_cache_get("alpha", "notes/a.md", allow_stale=False)
    assert content == "live-content"
    assert cached is not None
    assert cached[0] == "live-content"
    assert cached[1] is False


@pytest.mark.asyncio
async def test_fetch_overrides_skips_smoke_file(monkeypatch: pytest.MonkeyPatch):
    async def _list_files(_project: str):
        return ["override-smoke-test.json"]

    async def _read_file(*args, **kwargs):
        raise AssertionError("override-smoke-test.json should be skipped")

    monkeypatch.setattr(orchestrator, "list_files", _list_files)
    monkeypatch.setattr(orchestrator, "read_project_file", _read_file)
    entries = await orchestrator._fetch_overrides_from_memmcp(10)
    assert entries == []


def test_build_missing_memory_file_stub_override_smoke():
    stub = orchestrator._build_missing_memory_file_stub(
        orchestrator.OVERRIDE_PROJECT,
        "override-smoke-test.json",
    )
    assert stub is not None
    assert stub["kind"] == "override_smoke_test"
    assert stub["bootstrap"] is True


def test_build_missing_memory_file_stub_unknown_index_defaults_latest_name():
    stub = orchestrator._build_missing_memory_file_stub(
        "algotraderv2_rust",
        "index__custom_signal.json",
    )
    assert stub is not None
    assert stub["kind"] == "memory_index"
    assert stub["latest"] == "custom_signal__latest.json"


@pytest.mark.asyncio
async def test_should_skip_unchanged_latest_hash(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "MEMORY_WRITE_LATEST_HASH_DEDUP_ENABLED", True)
    monkeypatch.setattr(orchestrator, "MEMORY_WRITE_LATEST_HASH_DEDUP_MAX_KEYS", 10)
    orchestrator.memory_write_latest_hashes.clear()

    first = await orchestrator.should_skip_unchanged_latest_hash(
        "alpha",
        "metrics__latest.json",
        "aaa",
    )
    second = await orchestrator.should_skip_unchanged_latest_hash(
        "alpha",
        "metrics__latest.json",
        "aaa",
    )
    changed = await orchestrator.should_skip_unchanged_latest_hash(
        "alpha",
        "metrics__latest.json",
        "bbb",
    )
    assert first is False
    assert second is True
    assert changed is False


def test_build_hot_memory_rollup_file_preserves_directory():
    rollup_file = orchestrator.build_hot_memory_rollup_file("telemetry/queue__latest.json")
    assert rollup_file == "telemetry/_rollups/queue__latest__rollup.json"


@pytest.mark.asyncio
async def test_flush_hot_memory_rollups_emits_compact_snapshot(monkeypatch: pytest.MonkeyPatch):
    captured: list[dict[str, object]] = []

    async def _capture(item: dict[str, object]):
        captured.append(item)

    monkeypatch.setattr(orchestrator, "_enqueue_memory_bank_write", _capture)
    monkeypatch.setattr(orchestrator, "HOT_MEMORY_ROLLUP_FLUSH_SECS", 1.0)
    orchestrator.hot_memory_rollup_entries.clear()
    orchestrator.hot_memory_rollup_health.update(
        {
            "pendingKeys": 0,
            "totalBuffered": 0,
            "totalFlushed": 0,
            "totalSkippedUnchanged": 0,
            "lastFlushAt": None,
            "lastFlushCount": 0,
            "lastError": None,
        }
    )

    await orchestrator.enqueue_hot_memory_rollup(
        {
            "project": "alpha",
            "file": "telemetry/queue__latest.json",
            "summary": "queue depth snapshot",
            "topic_path": "telemetry",
            "topic_tags": ["telemetry"],
            "content_hash": "abc123",
            "content_length": 5120,
            "letta_session": None,
            "letta_context": {},
            "qdrant_collection": "memmcp_notes",
        }
    )

    result = await orchestrator.flush_hot_memory_rollups(force=True)
    assert result["flushed"] == 1
    assert len(captured) == 1

    payload = captured[0]["payload"]
    assert isinstance(payload, dict)
    assert payload["fileName"] == "telemetry/_rollups/queue__latest__rollup.json"
    assert "\"kind\": \"high_frequency_rollup\"" in payload["content"]
    assert "\"source_file\": \"telemetry/queue__latest.json\"" in payload["content"]


@pytest.mark.asyncio
async def test_write_memory_hot_file_buffers_then_skips_unchanged(monkeypatch: pytest.MonkeyPatch):
    buffered: list[dict[str, object]] = []

    async def _fake_summarize(content: str, max_length: int = 500):
        return content[:max_length]

    async def _persist_raw(event: dict[str, object]):
        return True, None

    async def _buffer(item: dict[str, object]):
        buffered.append(item)

    async def _fanout_summary():
        return {"by_status": {}, "by_target": {}}

    monkeypatch.setattr(orchestrator, "HOT_MEMORY_ROLLUP_ENABLED", True)
    monkeypatch.setattr(orchestrator, "HOT_MEMORY_FILE_SUFFIXES", ["__latest.json"])
    monkeypatch.setattr(orchestrator, "MEMORY_WRITE_LATEST_HASH_DEDUP_ENABLED", True)
    monkeypatch.setattr(orchestrator, "MEMORY_WRITE_LATEST_HASH_DEDUP_MAX_KEYS", 100)
    monkeypatch.setattr(orchestrator, "summarize_content", _fake_summarize)
    monkeypatch.setattr(orchestrator, "persist_raw_event_to_mongo", _persist_raw)
    monkeypatch.setattr(orchestrator, "enqueue_hot_memory_rollup", _buffer)
    monkeypatch.setattr(orchestrator, "get_fanout_summary", _fanout_summary)
    orchestrator.memory_write_latest_hashes.clear()

    request = SimpleNamespace(state=SimpleNamespace(request_id="test-hot"))
    payload = orchestrator.MemoryWrite(
        projectName="alpha",
        fileName="telemetry/queue__latest.json",
        content="{\"queueDepth\":42}",
    )

    first = await orchestrator.write_memory(payload, request)
    second = await orchestrator.write_memory(payload, request)

    assert first["ok"] is True
    assert first["rollup_buffered"] is True
    assert len(buffered) == 1
    assert second["ok"] is True
    assert second["deduped"] is True
    assert second["latest_hash_unchanged"] is True


def test_letta_transient_error_detection_and_threshold(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "LETTA_DISABLE_ON_TRANSIENT_ERRORS", True)
    monkeypatch.setattr(orchestrator, "LETTA_TRANSIENT_ERROR_THRESHOLD", 3)
    orchestrator.letta_transient_error_streak = 0

    assert orchestrator._is_letta_transient_server_error(
        "Letta sync failed: status=500 body={\"detail\":\"An unknown error occurred\"}"
    )
    assert orchestrator._record_letta_transient_failure("status=500 server error") is False
    assert orchestrator._record_letta_transient_failure("status=502 gateway error") is False
    assert orchestrator._record_letta_transient_failure("status=503 upstream error") is True
    assert orchestrator.letta_transient_error_streak == 3

    # Non-server failures reset the streak.
    assert orchestrator._record_letta_transient_failure("status=429 too many requests") is False
    assert orchestrator.letta_transient_error_streak == 0


def test_is_mcp_missing_session_error():
    assert orchestrator._is_mcp_missing_session_error(
        400,
        "Bad Request: No valid session ID provided",
    )
    assert orchestrator._is_mcp_missing_session_error(
        404,
        "session not found",
    )
    assert not orchestrator._is_mcp_missing_session_error(
        500,
        "internal server error",
    )


@pytest.mark.asyncio
async def test_call_mcp_reinitializes_session_when_gateway_rejects_session(
    monkeypatch: pytest.MonkeyPatch,
):
    class _FakeResponse:
        def __init__(self, status_code: int, text: str, headers: dict[str, str] | None = None):
            self.status_code = status_code
            self.text = text
            self.headers = headers or {}

        def json(self):
            return json.loads(self.text)

    ensure_calls: list[bool] = []

    async def _ensure(force_refresh: bool = False):
        ensure_calls.append(force_refresh)
        return "session-new" if force_refresh else "session-old"

    responses = [
        _FakeResponse(
            400,
            json.dumps(
                {
                    "jsonrpc": "2.0",
                    "error": {"code": -32000, "message": "Bad Request: No valid session ID provided"},
                    "id": None,
                }
            ),
        ),
        _FakeResponse(
            200,
            'event: message\ndata: {"jsonrpc":"2.0","id":"1","result":{"isError":false,"content":[]}}\n',
            {"mcp-session-id": "session-new"},
        ),
    ]

    async def _post(_payload: dict[str, object], session_id: str | None = None):
        assert session_id in ("session-old", "session-new")
        if not responses:
            raise AssertionError("unexpected extra MCP request")
        return responses.pop(0)

    monkeypatch.setattr(orchestrator, "_ensure_mcp_session", _ensure)
    monkeypatch.setattr(orchestrator, "_post_mcp_request", _post)
    orchestrator.MCP_SESSION_ID = "session-old"

    result = await orchestrator._call_mcp({"jsonrpc": "2.0", "id": "1", "method": "tools/list"})
    assert result["isError"] is False
    assert ensure_calls == [False, True]
    assert orchestrator.MCP_SESSION_ID == "session-new"


@pytest.mark.asyncio
async def test_rebuild_topic_rollups_dedupes_and_extracts_numeric_facts(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
):
    monkeypatch.setattr(orchestrator, "TOPIC_ROLLUP_PATH", tmp_path / "topic_rollups.json")
    monkeypatch.setattr(orchestrator, "TOPIC_ROLLUP_HISTORY_SCAN_LIMIT", 50)
    monkeypatch.setattr(orchestrator, "TOPIC_ROLLUP_MAX_SUMMARY_SNIPPETS", 8)
    monkeypatch.setattr(orchestrator, "TOPIC_ROLLUP_MAX_NUMERIC_FACTS", 16)
    monkeypatch.setattr(orchestrator, "TOPIC_ROLLUP_MAX_UNIQUE_FILES", 16)

    async with orchestrator.topic_tree_lock:
        orchestrator.topic_tree.clear()
        orchestrator.topic_tree.update(
            {
                "alpha": {
                    "count": 3,
                    "children": {
                        "decisions": {
                            "count": 3,
                            "children": {},
                        }
                    },
                }
            }
        )

    async with orchestrator.memory_write_history_lock:
        orchestrator.memory_write_history.clear()
        orchestrator.memory_write_history.extend(
            [
                {
                    "timestamp": "2026-03-02T17:00:00Z",
                    "project": "alpha",
                    "file": "decisions/a.md",
                    "topic_path": "decisions",
                    "summary": "PnL improved to 123.45 after retry budget change.",
                    "contentLength": 64,
                },
                {
                    "timestamp": "2026-03-02T17:00:00Z",
                    "project": "alpha",
                    "file": "decisions/a.md",
                    "topic_path": "decisions",
                    "summary": "PnL improved to 123.45 after retry budget change.",
                    "contentLength": 64,
                },
                {
                    "timestamp": "2026-03-02T17:01:00Z",
                    "project": "alpha",
                    "file": "decisions/b.md",
                    "topic_path": "decisions",
                    "summary": "Queue depth dropped by 7 in the latest run.",
                    "contentLength": 59,
                },
            ]
        )

    snapshot = await orchestrator.rebuild_topic_rollups_once()
    assert snapshot["historyEntriesScanned"] == 3
    assert snapshot["historyEntriesDeduped"] == 2

    alpha_topics = snapshot["projects"]["alpha"]["topics"]
    decisions = next(item for item in alpha_topics if item["path"] == "decisions")
    assert decisions["eventCount"] >= 3
    assert decisions["recentEventCount"] == 2
    assert decisions["uniqueFileCount"] == 2
    assert any(fact["value"] == "123.45" for fact in decisions["numericFacts"])
    assert any(fact["value"] == "7" for fact in decisions["numericFacts"])


@pytest.mark.asyncio
async def test_search_topic_rollups_returns_rollup_source_rows():
    async with orchestrator.topic_rollup_lock:
        orchestrator.topic_rollup_index.clear()
        orchestrator.topic_rollup_index.update(
            {
                "generatedAt": "2026-03-02T18:00:00Z",
                "historyEntriesScanned": 20,
                "historyEntriesDeduped": 12,
                "projects": {
                    "alpha": {
                        "topicCount": 1,
                        "topics": [
                            {
                                "path": "decisions/knobs",
                                "depth": 2,
                                "eventCount": 20,
                                "recentEventCount": 5,
                                "uniqueFileCount": 3,
                                "uniqueFiles": ["decisions/a.md"],
                                "latestTimestamp": "2026-03-02T17:59:00Z",
                                "summarySnippets": ["Expectancy improved after tighter stop-loss controls."],
                                "numericFacts": [
                                    {
                                        "value": "88.1%",
                                        "sourceFile": "decisions/a.md",
                                        "topicPath": "decisions/knobs",
                                        "timestamp": "2026-03-02T17:59:00Z",
                                        "snippet": "win rate reached 88.1% after the update",
                                    }
                                ],
                                "inference": [],
                                "children": [],
                            }
                        ],
                    }
                },
            }
        )

    rows = await orchestrator.search_topic_rollups(
        "win rate 88.1%",
        limit=5,
        project_filter="alpha",
        topic_filter="decisions",
    )
    assert rows
    assert rows[0]["source"] == orchestrator.RETRIEVAL_SOURCE_TOPIC_ROLLUPS
    assert rows[0]["topic_rollup"]["event_count"] == 20


@pytest.mark.asyncio
async def test_backfill_topic_rollups_sets_hold_window(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
):
    monkeypatch.setattr(orchestrator, "TOPIC_ROLLUP_PATH", tmp_path / "topic_rollups.json")
    monkeypatch.setattr(orchestrator, "TOPIC_ROLLUP_BACKFILL_HOLD_SECS", 120.0)
    monkeypatch.setattr(orchestrator, "topic_rollup_backfill_hold_until_monotonic", 0.0)

    async def _from_qdrant(*, scan_limit: int, project: str | None = None) -> list[dict[str, Any]]:
        assert scan_limit == 10
        assert project == "alpha"
        return [
            {
                "project": "alpha",
                "file": "decisions/a.md",
                "topic_path": "decisions",
                "summary": "Win rate improved to 55% after queue tuning.",
                "timestamp": "2026-03-02T18:20:00Z",
            }
        ]

    monkeypatch.setattr(orchestrator, "_topic_rollup_entries_from_qdrant", _from_qdrant)

    async with orchestrator.topic_tree_lock:
        orchestrator.topic_tree.clear()
        orchestrator.topic_tree.update({"alpha": {"count": 1, "children": {"decisions": {"count": 1, "children": {}}}}})

    result = await orchestrator.backfill_topic_rollups_once(source="qdrant", scan_limit=10, project="alpha")
    assert result["ok"] is True
    assert orchestrator.topic_rollup_health["lastSource"] == "backfill:qdrant"
    assert orchestrator.topic_rollup_health["lastBackfillSource"] == "qdrant"
    assert orchestrator.topic_rollup_health["lastBackfillProject"] == "alpha"
    assert orchestrator.topic_rollup_health["lastBackfillRowsScanned"] == 1
    assert orchestrator.topic_rollup_backfill_hold_until_monotonic > time.monotonic()
    assert orchestrator._topic_rollup_hold_remaining_secs() > 0
    assert orchestrator.topic_rollup_health["backfillHoldUntil"] is not None


@pytest.mark.asyncio
async def test_migration_runtime_status_disabled_when_runtime_unavailable(monkeypatch: pytest.MonkeyPatch):
    async def _none():
        return None

    monkeypatch.setattr(orchestrator, "_get_migration_runtime", _none)
    payload = await orchestrator.migration_runtime_status()
    assert payload["enabled"] is False
    assert isinstance(payload.get("flags"), dict)


@pytest.mark.asyncio
async def test_migration_runtime_status_reports_snapshot(monkeypatch: pytest.MonkeyPatch):
    class _FakeRuntime:
        implementation_map = {
            "codec": "RustCodecBridge",
            "memory_store": "RustMemoryStoreProxy",
            "retriever": "RustRetrieverProxy",
            "scheduler": "GoSchedulerProxy",
            "state_delta": "JsonMergeStateDelta",
        }

    async def _runtime():
        return _FakeRuntime()

    async def _snapshot(_runtime_obj):
        return {"retriever_health": {"ok": True}}

    monkeypatch.setattr(orchestrator, "_get_migration_runtime", _runtime)
    monkeypatch.setattr(orchestrator, "runtime_snapshot", _snapshot)
    payload = await orchestrator.migration_runtime_status()
    assert payload["enabled"] is True
    assert payload["implementations"]["retriever"] == "RustRetrieverProxy"
    assert payload["snapshot"]["retriever_health"]["ok"] is True


@pytest.mark.asyncio
async def test_scheduler_submit_via_runtime_uses_scheduler_adapter(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, Any] = {}

    class _FakeScheduler:
        async def submit_task(self, request):
            captured["title"] = request.title
            captured["project"] = request.project
            return {"id": "runtime-task", "status": "queued"}

    class _FakeRuntime:
        scheduler = _FakeScheduler()

    async def _runtime():
        return _FakeRuntime()

    monkeypatch.setattr(orchestrator, "_get_migration_runtime", _runtime)
    result = await orchestrator._scheduler_submit_via_runtime(
        title="runtime-test",
        project="alpha",
        agent="codex",
        priority=4,
        payload={"action": "memory_search", "query": "alpha"},
    )
    assert result["id"] == "runtime-task"
    assert captured["title"] == "runtime-test"
    assert captured["project"] == "alpha"


@pytest.mark.asyncio
async def test_engine_retrieval_health_endpoint():
    payload = await orchestrator.engine_retrieval_health()
    assert payload["ok"] is True
    assert payload["mode"] == "service-compat"


@pytest.mark.asyncio
async def test_engine_retrieval_query_with_grounding_routes_to_pipeline(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, Any] = {}

    async def _pipeline(**kwargs):
        captured.update(kwargs)
        return ([{"summary": "ok", "score": 1.0}], {"retrieval_mode": "balanced"}, [], {"facts": []})

    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)
    response = await orchestrator.engine_retrieval_query_with_grounding(
        {"request": {"query": "alpha", "limit": 4, "project_filter": "proj-a"}}
    )
    assert response["results"]
    assert captured["query"] == "alpha"
    assert captured["project_filter"] == "proj-a"
    assert captured["limit"] == 4


@pytest.mark.asyncio
async def test_engine_memory_get_returns_content(monkeypatch: pytest.MonkeyPatch):
    async def _read(project: str, file_name: str, **_kwargs):
        assert project == "alpha"
        assert file_name == "notes/a.md"
        return "content-body"

    monkeypatch.setattr(orchestrator, "read_project_file", _read)
    payload = await orchestrator.engine_memory_get("alpha::notes/a.md")
    memory = payload["memory"]
    assert memory["project"] == "alpha"
    assert memory["file_name"] == "notes/a.md"
    assert memory["content"] == "content-body"
