from __future__ import annotations

import asyncio
import importlib.util
import json
import sys
import time
from types import SimpleNamespace
from datetime import datetime, timedelta, timezone
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
orchestrator.RETRIEVAL_PATHWAY_NEGATIVE_CACHE_ENABLED = False


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


def test_merge_federated_rows_applies_code_context_adjustments(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "CODE_CONTEXT_ENRICH_ENABLED", True)
    monkeypatch.setattr(orchestrator, "CODE_CONTEXT_SYMBOL_OVERLAP_BOOST", 0.2)
    monkeypatch.setattr(orchestrator, "CODE_CONTEXT_FILEPATH_PROXIMITY_BOOST", 0.1)
    monkeypatch.setattr(orchestrator, "CODE_CONTEXT_RECENCY_MAX_BOOST", 0.05)
    rows = {
        "qdrant": [
            {
                "project": "alpha",
                "file": "services/orchestrator/app.py",
                "summary": "retrieval timeout and staged fetch tuning",
                "score": 0.42,
                "code_context": {
                    "symbols": ["retrieval", "timeout", "staged_fetch"],
                    "path_tokens": ["services", "orchestrator", "app", "py"],
                    "updated_at": orchestrator._utc_now(),
                },
            }
        ]
    }
    merged = orchestrator._merge_federated_rows(
        rows,
        {"qdrant": 1.0},
        set(),
        set(),
        learning_enabled=False,
        query="orchestrator retrieval timeout tuning",
    )
    assert len(merged) == 1
    row = merged[0]
    assert row["code_context_adjustment"] > 0
    assert row["score"] > row["base_score"]


def test_merge_federated_rows_suppresses_low_value_non_letta(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LOW_VALUE_NON_LETTA_SUPPRESS", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LOW_VALUE_NON_LETTA_PENALTY", 0.3)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LOW_VALUE_NON_LETTA_SCORE_CAP", 0.4)
    rows = {
        "qdrant": [
            {
                "project": "alpha",
                "file": "telemetry/queue__latest.json",
                "summary": "queue depth reached 220",
                "score": 0.82,
            }
        ],
        "letta": [
            {
                "project": "alpha",
                "file": "telemetry/queue-history.log",
                "summary": "queue depth reached 220",
                "score": 0.76,
            }
        ],
    }
    merged = orchestrator._merge_federated_rows(
        rows,
        {"qdrant": 1.0, "letta": 1.0},
        set(),
        set(),
        learning_enabled=False,
        query="how did recall quality improve",
    )
    by_source = {row.get("source"): row for row in merged}
    assert by_source["qdrant"]["low_value_suppressed"] is True
    assert by_source["qdrant"]["score"] <= 0.4
    assert by_source["letta"]["low_value_suppressed"] is False


def test_letta_root_json_low_value_helpers(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(
        orchestrator,
        "LETTA_LOW_VALUE_ROOT_JSON_PREFIXES",
        ["arena__", "risk__", "dex__"],
    )
    assert orchestrator._looks_letta_root_json_low_value(
        "arena__weights__20260312.json",
        "root",
    )
    assert orchestrator._looks_letta_root_json_low_value(
        "risk__circuit_breaker__20260312.json",
        "",
    )
    assert not orchestrator._looks_letta_root_json_low_value(
        "decisions/trade-plan.md",
        "runbooks/profitability",
    )
    excluded, reason = orchestrator._is_letta_excluded_memory_record(
        "dex__router_snapshot-20260312T185520494Z.json",
        "root",
    )
    assert excluded is True
    assert reason == "excluded_root_json_prefix"


def test_mindsdb_decode_classifier_includes_flatbuffer_footer():
    assert orchestrator._is_mindsdb_lz4_decompress_error(
        RuntimeError("[file/files]: Verification of flatbuffer-encoded Footer failed.")
    )
    assert orchestrator._is_mindsdb_lz4_decompress_error(
        RuntimeError("LZ4 decompress failed: ERROR_decompressionFailed")
    )
    assert not orchestrator._is_mindsdb_lz4_decompress_error(
        RuntimeError("network reset by peer")
    )


def test_runner_capability_map_payload(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "MCP_CAPABILITY_MAP_ENABLED", True)
    monkeypatch.setattr(
        orchestrator,
        "MIGRATION_FLAGS",
        SimpleNamespace(
            use_go_orchestrator=True,
            use_rust_codec=True,
            use_rust_memory=True,
            use_rust_retrieval=True,
        ),
    )
    payload = orchestrator._runner_capability_map_payload()
    assert payload["enabled"] is True
    assert payload["defaultRunner"] == "go_scheduler"
    assert payload["tools"]["memory_write_batch"] is True
    assert payload["runnerContracts"]["statusLifecycle"] == [
        "queued",
        "claimed",
        "partial",
        "succeeded",
        "failed",
    ]


@pytest.mark.asyncio
async def test_write_browser_context_routes_to_memory_write(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "BROWSER_CONTEXT_INGEST_ENABLED", True)
    captured: dict[str, Any] = {}

    async def _fake_write_memory(payload, request):
        captured["payload"] = payload
        captured["path"] = request.url.path
        return {"ok": True, "event_id": "evt-1", "warnings": [], "fanout": {}}

    monkeypatch.setattr(orchestrator, "write_memory", _fake_write_memory)
    payload = orchestrator.BrowserContextIngest(
        projectName="algotraderv2_rust",
        pageUrl="https://example.com/docs/retrieval",
        title="Retrieval Tuning",
        textSnapshot="staged fetch works with async warm",
        topicPath="runbooks/performance",
        agentId="codex",
    )
    result = await orchestrator.write_browser_context(payload)
    write_payload = captured["payload"]
    assert write_payload.projectName == "algotraderv2_rust"
    assert write_payload.fileName.startswith("browser/example-com/")
    assert write_payload.topicPath == "runbooks/performance"
    assert "browser context persisted" in " ".join(result.get("warnings") or []).lower()
    assert result["browserContext"]["url"] == "https://example.com/docs/retrieval"


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
async def test_federated_search_call_budget_defers_slow_sources(monkeypatch: pytest.MonkeyPatch):
    warm_calls: list[dict[str, Any]] = []

    async def _qdrant(*args, **kwargs):
        return [
            {
                "project": "alpha",
                "file": "notes/a.md",
                "summary": "fast qdrant row",
                "score": 0.88,
                "source": "qdrant",
            }
        ]

    async def _letta(*args, **kwargs):
        await asyncio.sleep(0.3)
        return [
            {
                "project": "alpha",
                "file": "notes/letta.md",
                "summary": "slow letta row",
                "score": 0.81,
                "source": "letta",
            }
        ]

    async def _empty(*args, **kwargs):
        return []

    def _warm(**kwargs):
        warm_calls.append(dict(kwargs))

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "_schedule_background_letta_query_warm", _warm)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)

    results, debug, warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT, orchestrator.RETRIEVAL_SOURCE_LETTA],
        rerank_with_learning=False,
        call_budget_secs=0.25,
    )

    assert results
    assert results[0]["source"] == "qdrant"
    assert "letta" in debug["call_budget"]["deferred_sources"]
    assert debug["call_budget"]["exhausted"] is True
    assert debug["call_budget"]["cacheWriteSkipped"] is True
    assert warm_calls
    assert any("deferred" in item.lower() for item in warnings)


@pytest.mark.asyncio
async def test_federated_search_timeout_fail_open_continues_slow_source(monkeypatch: pytest.MonkeyPatch):
    scheduled: list[dict[str, Any]] = []

    async def _slow_memory_bank(*args, **kwargs):
        await asyncio.sleep(1.2)
        return []

    def _schedule(**kwargs):
        scheduled.append(dict(kwargs))

    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _slow_memory_bank)
    monkeypatch.setattr(orchestrator, "_schedule_background_source_warm", _schedule)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_TIMEOUT_SECS", 1.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_ENABLED", True)
    monkeypatch.setattr(
        orchestrator,
        "RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK],
    )

    _results, debug, warnings = await orchestrator.federated_search_memory(
        "alpha timeout memory path",
        limit=3,
        sources=[orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK],
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )

    assert scheduled
    assert scheduled[0]["source"] == orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK
    assert scheduled[0]["force"] is True
    assert debug["staged_fetch"]["fail_open_continuation_sources"] == [
        orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK
    ]
    assert any("continuing asynchronously for cache warm" in item.lower() for item in warnings)


@pytest.mark.asyncio
async def test_federated_search_deep_mode_fail_open_nonblocking_defaults(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0, "letta": 0}
    scheduled: list[str] = []

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [
            {
                "project": "alpha",
                "file": "notes/fast.md",
                "summary": "fast source result",
                "score": 0.95,
                "source": "qdrant",
            }
        ]

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return [
            {
                "project": "alpha",
                "file": "notes/slow.md",
                "summary": "slow source result",
                "score": 0.55,
                "source": "letta",
            }
        ]

    async def _empty(*args, **kwargs):
        return []

    def _schedule_letta(*, source: str, **_kwargs):
        scheduled.append(source)

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "_schedule_background_source_warm", _schedule_letta)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_DEEP_BLOCKING", False)
    monkeypatch.setattr(
        orchestrator,
        "RETRIEVAL_SOURCES_ENV",
        ",".join([orchestrator.RETRIEVAL_SOURCE_QDRANT, orchestrator.RETRIEVAL_SOURCE_LETTA]),
    )
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

    results, debug, _warnings = await orchestrator.federated_search_memory(
        "alpha deep read",
        limit=1,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="deep",
    )

    assert results
    assert calls["qdrant"] == 1
    assert calls["letta"] == 0
    assert debug["staged_fetch"]["hard_sync_async_split"] is True
    assert debug["staged_fetch"]["force_include_slow"] is False
    assert orchestrator.RETRIEVAL_SOURCE_LETTA in debug["staged_fetch"]["async_warm_sources"]
    assert orchestrator.RETRIEVAL_SOURCE_LETTA in scheduled


@pytest.mark.asyncio
async def test_federated_search_applies_qdrant_sync_timeout_cap(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0, "rollups": 0}

    async def _slow_qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        await asyncio.sleep(1.2)
        return [{"project": "alpha", "file": "q/a.md", "summary": "late qdrant", "score": 0.7, "source": "qdrant"}]

    async def _rollups(*args, **kwargs):
        calls["rollups"] += 1
        return [{"project": "alpha", "file": "r/a.md", "summary": "fast rollup", "score": 0.9, "source": "topic_rollups"}]

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _slow_qdrant)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _rollups)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _empty)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "_schedule_background_source_warm", lambda **_kwargs: None)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_QDRANT_TIMEOUT_SECS", 8.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_QDRANT_SYNC_TIMEOUT_CAP_SECS", 1.0)
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_FAST_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_TOPIC_ROLLUPS, orchestrator.RETRIEVAL_SOURCE_QDRANT],
    )
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_SLOW_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_LETTA, orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK],
    )

    started = time.monotonic()
    results, _debug, warnings = await orchestrator.federated_search_memory(
        "alpha qdrant timeout cap",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )
    elapsed = time.monotonic() - started

    assert results
    assert calls["qdrant"] == 1
    assert calls["rollups"] == 1
    assert elapsed < 1.5
    assert any("qdrant retrieval failed" in item.lower() for item in warnings)


@pytest.mark.asyncio
async def test_federated_search_suppresses_slow_timeout_warning_when_fast_sources_succeed(
    monkeypatch: pytest.MonkeyPatch,
):
    async def _rollups(*args, **kwargs):
        return [
            {
                "project": "alpha",
                "file": "rollup.md",
                "summary": "fast row",
                "score": 0.91,
                "source": "topic_rollups",
            }
        ]

    async def _slow_mindsdb(*args, **kwargs):
        await asyncio.sleep(1.2)
        return []

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_topic_rollups", _rollups)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _slow_mindsdb)
    monkeypatch.setattr(orchestrator, "search_qdrant", _empty)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _empty)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "_schedule_background_source_warm", lambda **_kwargs: None)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_MIN_FAST_RESULTS", 2)
    monkeypatch.setattr(
        orchestrator,
        "RETRIEVAL_SYNC_ASYNC_FALLBACK_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_MINDSDB],
    )
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_SLOW_REQUIRES_EXPLICIT", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_STRICT_FAST_SYNC_DEFAULT", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MINDSDB_TIMEOUT_SECS", 0.2)
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_FAST_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_TOPIC_ROLLUPS],
    )
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_SLOW_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_MINDSDB],
    )

    _results, debug, warnings = await orchestrator.federated_search_memory(
        "alpha staged suppression",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )

    joined = " | ".join(str(item) for item in warnings).lower()
    assert "mindsdb retrieval failed" not in joined
    assert "sources returned now: topic_rollups." in joined
    assert "additional context may be available later from: mindsdb." in joined
    mindsdb_error = debug["source_errors"]["mindsdb"]
    assert isinstance(mindsdb_error, dict)
    assert mindsdb_error.get("kind") == "budget_exceeded"
    assert mindsdb_error.get("timeout") is False


@pytest.mark.asyncio
async def test_retrieval_source_quality_endpoint_uses_rolling_window(monkeypatch: pytest.MonkeyPatch):
    async def _metrics(_limit: int):
        return {
            "updatedAt": "2026-03-14T00:00:00Z",
            "recallQuality": {
                "bySource": {
                    "qdrant": {"errorRate": 0.1, "requests": 10},
                    "mindsdb": {"errorRate": 0.2, "requests": 10},
                }
            },
            "latency": {
                "sources": {
                    "qdrant": {"requests": 100, "timeouts": 10, "p50Ms": 100.0, "p95Ms": 400.0, "p99Ms": 600.0},
                    "mindsdb": {"requests": 80, "timeouts": 8, "p50Ms": 90.0, "p95Ms": 500.0, "p99Ms": 700.0},
                }
            },
            "sourceCircuit": {},
            "backlogGating": {"enabled": True, "lettaOutstandingMax": 700},
        }

    monkeypatch.setattr(orchestrator, "_build_retrieval_metrics_payload", _metrics)

    now = datetime.now(timezone.utc)
    recent_ts = (now - timedelta(minutes=5)).isoformat().replace("+00:00", "Z")
    old_ts = (now - timedelta(hours=2)).isoformat().replace("+00:00", "Z")
    async with orchestrator.recall_quality_lock:
        existing = list(orchestrator.recall_quality_history)
        orchestrator.recall_quality_history.clear()
        orchestrator.recall_quality_history.append(
            {
                "timestamp": old_ts,
                "sourceCounts": {"qdrant": 50},
                "sourceErrorKinds": {"qdrant": "timeout"},
                "sourceErrors": {"qdrant": "qdrant retrieval timed out"},
                "sources": ["qdrant"],
            }
        )
        orchestrator.recall_quality_history.append(
            {
                "timestamp": recent_ts,
                "sourceCounts": {"qdrant": 4, "mindsdb": 2},
                "sourceErrorKinds": {"qdrant": "timeout", "mindsdb": "budget_exceeded"},
                "sourceErrors": {
                    "qdrant": {"kind": "timeout", "error": "qdrant retrieval timed out"},
                    "mindsdb": {"kind": "budget_exceeded", "error": "mindsdb retrieval sync budget exceeded"},
                },
                "sources": ["qdrant", "mindsdb"],
            }
        )
    try:
        payload = await orchestrator.get_retrieval_source_quality(limit=20, window_mins=30)
    finally:
        async with orchestrator.recall_quality_lock:
            orchestrator.recall_quality_history.clear()
            for row in existing:
                orchestrator.recall_quality_history.append(row)

    assert payload["window"]["sampleCount"] == 1
    rows = {row["source"]: row for row in payload["sources"]}
    assert rows["qdrant"]["requests"] == 4
    assert rows["qdrant"]["timeouts"] == 1
    assert rows["qdrant"]["timeoutRate"] == 0.25
    assert rows["mindsdb"]["budgetExceeded"] == 1
    assert rows["mindsdb"]["budgetExceededRate"] == 0.5
    assert rows["mindsdb"]["timeoutRate"] == 0.0
    assert isinstance(payload.get("lifetimeSources"), list)


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
    assert max(fake_client.calls) >= 3.0
    assert max(fake_client.calls) <= 12.0

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
async def test_federated_search_deep_mode_defers_mongo_raw_for_non_raw_intent(
    monkeypatch: pytest.MonkeyPatch,
):
    calls = {"qdrant": 0, "mongo_raw": 0}
    warmed: list[str] = []

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [
            {
                "project": "alpha",
                "file": f"fast/{idx}.md",
                "summary": "high-confidence answer from fast source",
                "score": 0.95 - (idx * 0.01),
                "source": "qdrant",
            }
            for idx in range(8)
        ]

    async def _mongo(*args, **kwargs):
        calls["mongo_raw"] += 1
        return [{"project": "alpha", "file": "raw/a.log", "summary": "raw row", "score": 0.4, "source": "mongo_raw"}]

    async def _empty(*args, **kwargs):
        return []

    def _warm(**kwargs):
        warmed.append(str(kwargs.get("source") or ""))

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _mongo)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _empty)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "_schedule_background_source_warm", _warm)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_WARM_SLOW_SOURCES", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_RESULTS", 2)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_TOP_SCORE", 0.55)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_DIVERSITY", 1)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MONGO_RAW_DEEP_SYNC_ONLY_FOR_RAW_INTENT", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MONGO_RAW_DEEP_ASYNC_WARM_NON_RAW", True)
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

    _, debug, warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="deep",
        retrieval_intent="decision",
    )

    assert calls["qdrant"] == 1
    assert calls["mongo_raw"] == 0
    assert orchestrator.RETRIEVAL_SOURCE_MONGO_RAW in debug["staged_fetch"]["async_warm_sources"]
    assert orchestrator.RETRIEVAL_SOURCE_MONGO_RAW in debug["source_policy"]["mongo_raw_intent_async_sources"]
    assert orchestrator.RETRIEVAL_SOURCE_MONGO_RAW in warmed
    assert any("mongo_raw deep retrieval ran asynchronously" in warning for warning in warnings)


@pytest.mark.asyncio
async def test_federated_search_deep_mode_keeps_mongo_raw_sync_for_raw_intent(
    monkeypatch: pytest.MonkeyPatch,
):
    calls = {"qdrant": 0, "mongo_raw": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [{"project": "alpha", "file": "fast/a.md", "summary": "fast result", "score": 0.75, "source": "qdrant"}]

    async def _mongo(*args, **kwargs):
        calls["mongo_raw"] += 1
        return [{"project": "alpha", "file": "raw/a.log", "summary": "raw row", "score": 0.68, "source": "mongo_raw"}]

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _mongo)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _empty)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MONGO_RAW_DEEP_SYNC_ONLY_FOR_RAW_INTENT", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MONGO_RAW_DEEP_ASYNC_WARM_NON_RAW", True)
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

    _, debug, _ = await orchestrator.federated_search_memory(
        "alpha raw logs",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="deep",
        retrieval_intent="raw",
    )

    assert calls["qdrant"] == 1
    assert calls["mongo_raw"] == 1
    assert debug["source_policy"]["mongo_raw_intent_async_sources"] == []


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
async def test_get_retrieval_template_caps_memory_timeout(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_TIMEOUT_SECS", 120.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_TIMEOUT_CAP_SECS", 9.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_DEEP_TIMEOUT_CAP_SECS", 17.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_TEMPLATE_CACHE_ENABLED", True)
    async with orchestrator.retrieval_template_cache_lock:
        orchestrator.retrieval_template_cache.clear()

    balanced, _ = await orchestrator._get_retrieval_template(
        "balanced",
        [orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK],
    )
    deep, _ = await orchestrator._get_retrieval_template(
        "deep",
        [orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK],
    )

    assert balanced["source_timeouts"][orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK] == 9.0
    assert deep["source_timeouts"][orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK] == 17.0


@pytest.mark.asyncio
async def test_recall_pipeline_scoped_query_caps_variants_and_escalation(monkeypatch: pytest.MonkeyPatch):
    calls: list[dict[str, Any]] = []

    async def _federated(*args, **kwargs):
        calls.append(dict(kwargs))
        return (
            [],
            {
                "retrieval_mode": kwargs.get("retrieval_mode"),
                "source_errors": {},
                "source_counts": {},
                "call_budget": {"exhausted": False, "deferred_sources": []},
            },
            [],
        )

    monkeypatch.setattr(orchestrator, "federated_search_memory", _federated)
    monkeypatch.setattr(orchestrator, "_expand_query_variants", lambda _query: ["v1", "v2", "v3", "v4"])
    monkeypatch.setattr(orchestrator, "AGENT_RECALL_ESCALATION_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RECALL_SCOPED_QUERY_VARIANT_CAP", 2)
    monkeypatch.setattr(orchestrator, "RECALL_SCOPED_QUERY_MAX_ESCALATION_STEPS", 1)

    _results, debug, _warnings, _grounding = await orchestrator._run_memory_recall_pipeline(
        query="alpha",
        limit=5,
        project_filter="algotraderv2_rust",
        topic_filter=None,
        sources=None,
        source_weights=None,
        preferences=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
        agent_profile=None,
        auto_escalate=True,
        query_expansion=True,
    )

    assert len(calls) == 2
    assert debug["pipeline"]["query_expansion"]["max_variants"] == 2
    assert debug["pipeline"]["escalation"]["max_steps"] == 1
    assert debug["pipeline"]["query_expansion"]["scoped_query"] is True


@pytest.mark.asyncio
async def test_recall_pipeline_scoped_query_filters_structural_variants(monkeypatch: pytest.MonkeyPatch):
    captured_queries: list[str] = []

    async def _federated(*args, **kwargs):
        captured_queries.append(str(args[0]))
        return (
            [],
            {
                "retrieval_mode": kwargs.get("retrieval_mode"),
                "source_errors": {},
                "source_counts": {},
                "call_budget": {"exhausted": False, "deferred_sources": []},
            },
            [],
        )

    monkeypatch.setattr(orchestrator, "federated_search_memory", _federated)
    monkeypatch.setattr(
        orchestrator,
        "_expand_query_variants",
        lambda _query: [
            "profitability tuning baseline ladder",
            "profitability/tuning/baseline/ladder",
            "profitability_tuning_baseline_ladder",
        ],
    )
    monkeypatch.setattr(orchestrator, "AGENT_RECALL_ESCALATION_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RECALL_SCOPED_QUERY_VARIANT_CAP", 3)

    _results, debug, _warnings, _grounding = await orchestrator._run_memory_recall_pipeline(
        query="profitability tuning baseline ladder",
        limit=5,
        project_filter="algotraderv2_rust",
        topic_filter=None,
        sources=None,
        source_weights=None,
        preferences=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
        agent_profile=None,
        auto_escalate=False,
        query_expansion=True,
    )

    assert captured_queries == ["profitability tuning baseline ladder"]
    assert debug["pipeline"]["query_expansion"]["scoped_structural_variant_filtered"] is True


@pytest.mark.asyncio
async def test_recall_pipeline_fast_mode_caps_query_variants_and_budget(monkeypatch: pytest.MonkeyPatch):
    calls: list[dict[str, Any]] = []

    async def _federated(*args, **kwargs):
        calls.append(dict(kwargs))
        return (
            [],
            {
                "retrieval_mode": kwargs.get("retrieval_mode"),
                "source_errors": {},
                "source_counts": {},
                "call_budget": {"exhausted": False, "deferred_sources": []},
            },
            [],
        )

    monkeypatch.setattr(orchestrator, "federated_search_memory", _federated)
    monkeypatch.setattr(orchestrator, "_expand_query_variants", lambda _query: ["v1", "v2", "v3"])
    monkeypatch.setattr(orchestrator, "AGENT_RECALL_ESCALATION_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RECALL_E2E_BUDGET_FAST_SECS", 13.0)
    monkeypatch.setattr(orchestrator, "RECALL_E2E_BUDGET_BALANCED_SECS", 60.0)
    monkeypatch.setattr(orchestrator, "RECALL_E2E_BUDGET_DEEP_SECS", 75.0)

    _results, debug, _warnings, _grounding = await orchestrator._run_memory_recall_pipeline(
        query="alpha",
        limit=5,
        project_filter="algotraderv2_rust",
        topic_filter=None,
        sources=None,
        source_weights=None,
        preferences=None,
        rerank_with_learning=False,
        retrieval_mode="fast",
        agent_profile=None,
        auto_escalate=False,
        query_expansion=True,
    )

    assert len(calls) == 1
    assert calls[0]["explicit_source_override"] is False
    assert debug["pipeline"]["query_expansion"]["max_variants"] == 1
    assert debug["pipeline"]["budget"]["configuredSecs"] == 13.0


@pytest.mark.asyncio
async def test_recall_pipeline_respects_e2e_budget(monkeypatch: pytest.MonkeyPatch):
    calls = {"count": 0}

    async def _federated(*args, **kwargs):
        calls["count"] += 1
        await asyncio.sleep(0.12)
        return (
            [
                {
                    "project": "alpha",
                    "file": "notes/a.md",
                    "summary": "budgeted row",
                    "score": 0.7,
                    "source": "qdrant",
                }
            ],
            {
                "retrieval_mode": kwargs.get("retrieval_mode"),
                "source_errors": {},
                "source_counts": {"qdrant": 1},
                "call_budget": {"exhausted": False, "deferred_sources": []},
            },
            [],
        )

    monkeypatch.setattr(orchestrator, "federated_search_memory", _federated)
    monkeypatch.setattr(orchestrator, "_expand_query_variants", lambda _query: ["v1", "v2", "v3"])
    monkeypatch.setattr(orchestrator, "RECALL_E2E_BUDGET_SECS", 0.15)
    monkeypatch.setattr(orchestrator, "RECALL_E2E_BUDGET_BALANCED_SECS", 0.15)
    monkeypatch.setattr(orchestrator, "RECALL_E2E_MIN_FEDERATED_BUDGET_SECS", 0.05)

    _results, debug, warnings, _grounding = await orchestrator._run_memory_recall_pipeline(
        query="alpha",
        limit=5,
        project_filter=None,
        topic_filter=None,
        sources=None,
        source_weights=None,
        preferences=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
        agent_profile=None,
        auto_escalate=False,
        query_expansion=True,
    )

    assert calls["count"] == 1
    assert debug["pipeline"]["budget"]["exhausted"] is True
    assert any("budget" in item.lower() for item in warnings)


@pytest.mark.asyncio
async def test_recall_pipeline_skips_timed_out_sources_on_followup_variants(
    monkeypatch: pytest.MonkeyPatch,
):
    call_sources: list[list[str] | None] = []

    async def _federated(*args, **kwargs):
        call_sources.append(kwargs.get("sources"))
        if len(call_sources) == 1:
            return (
                [],
                {
                    "retrieval_mode": kwargs.get("retrieval_mode"),
                    "source_errors": {"qdrant": "qdrant retrieval timed out after 4.0s"},
                    "source_counts": {"topic_rollups": 1},
                    "call_budget": {"exhausted": False, "deferred_sources": []},
                },
                [],
            )
        return (
            [],
            {
                "retrieval_mode": kwargs.get("retrieval_mode"),
                "source_errors": {},
                "source_counts": {"topic_rollups": 1},
                "call_budget": {"exhausted": False, "deferred_sources": []},
            },
            [],
        )

    monkeypatch.setattr(orchestrator, "federated_search_memory", _federated)
    monkeypatch.setattr(orchestrator, "_expand_query_variants", lambda _query: ["v1", "v2"])
    monkeypatch.setattr(orchestrator, "AGENT_RECALL_ESCALATION_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RECALL_TIMEOUT_ADAPTIVE_SOURCE_SKIP_ENABLED", True)
    monkeypatch.setattr(
        orchestrator,
        "RECALL_TIMEOUT_ADAPTIVE_SKIP_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_QDRANT],
    )

    _results, debug, warnings, _grounding = await orchestrator._run_memory_recall_pipeline(
        query="alpha",
        limit=5,
        project_filter=None,
        topic_filter=None,
        sources=None,
        source_weights=None,
        preferences=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
        agent_profile=None,
        auto_escalate=False,
        query_expansion=True,
    )

    assert len(call_sources) == 2
    assert call_sources[1] is not None
    assert orchestrator.RETRIEVAL_SOURCE_QDRANT not in call_sources[1]
    assert debug["pipeline"]["timeout_adaptive"]["enabled"] is True
    assert debug["pipeline"]["timeout_adaptive"]["observed_timeout_sources"] == [
        orchestrator.RETRIEVAL_SOURCE_QDRANT
    ]
    assert debug["pipeline"]["timeout_adaptive"]["skipped_sources"] == [
        orchestrator.RETRIEVAL_SOURCE_QDRANT
    ]
    assert any("adaptive timeout policy skipped timed-out sources" in item.lower() for item in warnings)


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


def test_retrieval_pathway_cache_backend_redis_mirror_is_write_only(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_BACKEND", "redis_mirror")
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_REDIS_URL", "redis://cache.local:6379/0")
    monkeypatch.setattr(orchestrator, "redis_async", object())
    assert orchestrator._retrieval_pathway_cache_backend_mode() == "redis_mirror"
    assert orchestrator._retrieval_pathway_cache_backend_enabled() is True
    assert orchestrator._retrieval_pathway_cache_backend_read_enabled() is False
    assert orchestrator._retrieval_pathway_cache_backend_write_enabled() is True


@pytest.mark.asyncio
async def test_retrieval_pathway_cache_backend_get_skips_in_redis_mirror_mode(
    monkeypatch: pytest.MonkeyPatch,
):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_BACKEND", "redis_mirror")
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_REDIS_URL", "redis://cache.local:6379/0")
    monkeypatch.setattr(orchestrator, "redis_async", object())
    called = {"client": 0}

    async def _client():
        called["client"] += 1
        return object()

    monkeypatch.setattr(orchestrator, "_get_retrieval_pathway_redis_client", _client)
    payload = await orchestrator._retrieval_pathway_cache_backend_get("abc")
    assert payload is None
    assert called["client"] == 0


@pytest.mark.asyncio
async def test_retrieval_pathway_cache_backend_set_writes_in_redis_mirror_mode(
    monkeypatch: pytest.MonkeyPatch,
):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_BACKEND", "redis_mirror")
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_REDIS_URL", "redis://cache.local:6379/0")
    monkeypatch.setattr(orchestrator, "redis_async", object())
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_REDIS_TIMEOUT_SECS", 0.5)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_TTL_SECS", 60.0)
    monkeypatch.setattr(orchestrator, "retrieval_pathway_cache_backend_writes", 0)
    captured: dict[str, Any] = {}

    class _FakeRedis:
        async def set(self, key: str, payload: bytes, ex: int):
            captured["key"] = key
            captured["payload"] = payload
            captured["ex"] = ex
            return True

    async def _client():
        return _FakeRedis()

    monkeypatch.setattr(orchestrator, "_get_retrieval_pathway_redis_client", _client)
    await orchestrator._retrieval_pathway_cache_backend_set(
        "write-through-key",
        results=[{"project": "alpha", "file": "notes/a.md", "summary": "cached", "score": 0.7}],
        retrieval_debug={"cache": {"pathway_hit": False}},
        warnings=["cached"],
    )
    assert captured["key"].endswith("write-through-key")
    assert captured["ex"] == 60
    assert orchestrator.retrieval_pathway_cache_backend_writes == 1


@pytest.mark.asyncio
async def test_federated_search_stale_cache_hit_schedules_swr_refresh(monkeypatch: pytest.MonkeyPatch):
    schedule_calls: list[dict[str, Any]] = []
    source_calls = {"qdrant": 0}

    async def _qdrant(*args, **kwargs):
        source_calls["qdrant"] += 1
        return []

    def _schedule(**kwargs):
        schedule_calls.append(dict(kwargs))

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "_schedule_retrieval_pathway_swr_refresh", _schedule)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_TTL_SECS", 10.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_STALE_WHILE_REVALIDATE_SECS", 120.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_MAX_KEYS", 128)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_NEGATIVE_CACHE_ENABLED", False)
    async with orchestrator.retrieval_pathway_cache_lock:
        orchestrator.retrieval_pathway_cache.clear()

    normalized_weights = orchestrator._normalize_retrieval_weights(None)
    cache_key = orchestrator._retrieval_pathway_cache_key(
        query="alpha stale query",
        limit=5,
        project_filter=None,
        topic_filter=None,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT],
        source_weights=normalized_weights,
        retrieval_mode="balanced",
        retrieval_intent="decision",
        learning_enabled=False,
        positive_terms=set(),
        negative_terms=set(),
    )
    now = time.monotonic()
    async with orchestrator.retrieval_pathway_cache_lock:
        orchestrator.retrieval_pathway_cache[cache_key] = {
            "expires_at": now - 1.0,
            "stale_until": now + 60.0,
            "value": {
                "results": [
                    {
                        "project": "alpha",
                        "file": "notes/stale.md",
                        "summary": "stale cached row",
                        "score": 0.72,
                        "source": "qdrant",
                    }
                ],
                "retrieval_debug": {"cache": {}},
                "warnings": [],
            },
        }

    results, debug, _warnings = await orchestrator.federated_search_memory(
        "alpha stale query",
        limit=5,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT],
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )
    assert results
    assert debug["cache"]["pathway_hit"] is True
    assert debug["cache"]["pathway_stale"] is True
    assert source_calls["qdrant"] == 0
    assert schedule_calls


@pytest.mark.asyncio
async def test_federated_search_negative_cache_short_circuits_repeat_empty_queries(monkeypatch: pytest.MonkeyPatch):
    source_calls = {"qdrant": 0}

    async def _qdrant(*args, **kwargs):
        source_calls["qdrant"] += 1
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_NEGATIVE_CACHE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_NEGATIVE_CACHE_TTL_SECS", 120.0)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_NEGATIVE_CACHE_MAX_KEYS", 128)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_NEGATIVE_CACHE_MIN_QUERY_CHARS", 3)
    async with orchestrator.retrieval_pathway_negative_cache_lock:
        orchestrator.retrieval_pathway_negative_cache.clear()
        orchestrator.retrieval_pathway_negative_cache_hits = 0
        orchestrator.retrieval_pathway_negative_cache_misses = 0

    _first_results, _first_debug, _first_warnings = await orchestrator.federated_search_memory(
        "alpha missing",
        limit=5,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT],
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )
    second_results, second_debug, _second_warnings = await orchestrator.federated_search_memory(
        "alpha missing",
        limit=5,
        sources=[orchestrator.RETRIEVAL_SOURCE_QDRANT],
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )
    assert second_results == []
    assert source_calls["qdrant"] == 1
    assert second_debug["cache"]["pathway_negative_hit"] is True
    assert orchestrator.retrieval_pathway_negative_cache_hits >= 1


@pytest.mark.asyncio
async def test_federated_search_intentional_slow_deferral_does_not_emit_partial_budget_warning(
    monkeypatch: pytest.MonkeyPatch,
):
    source_calls = {"qdrant": 0, "letta": 0}

    async def _qdrant(*args, **kwargs):
        source_calls["qdrant"] += 1
        return [
            {
                "project": "alpha",
                "file": "notes/fast.md",
                "summary": "fast row",
                "score": 0.84,
                "source": "qdrant",
            }
        ]

    async def _letta(*args, **kwargs):
        source_calls["letta"] += 1
        return []

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_SLOW_REQUIRES_EXPLICIT", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_STRICT_FAST_SYNC_DEFAULT", True)
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

    _results, debug, warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )
    assert source_calls["qdrant"] == 1
    assert source_calls["letta"] == 0
    assert debug["call_budget"]["cacheWriteSkipped"] is False
    assert not any("partial results due call budget" in item.lower() for item in warnings)


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
async def test_federated_search_hard_sync_async_split_uses_fallback_source(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0, "letta": 0, "memory_bank": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [
            {
                "project": "alpha",
                "file": "fast/a.md",
                "summary": "fast answer",
                "score": 0.81,
                "source": "qdrant",
            }
        ]

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return []

    async def _memory_bank(*args, **kwargs):
        calls["memory_bank"] += 1
        return [
            {
                "project": "alpha",
                "file": "slow/memory.md",
                "summary": "fallback answer",
                "score": 0.44,
                "source": "memory_bank",
            }
        ]

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _memory_bank)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_DEEP_BLOCKING", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_MIN_FAST_RESULTS", 2)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_FALLBACK_SOURCES", [orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK])
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_SYNC_NON_DEEP_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_SLOW_REQUIRES_EXPLICIT", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_STRICT_FAST_SYNC_DEFAULT", False)
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

    results, debug, _warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )

    assert results
    assert calls["qdrant"] == 1
    assert calls["letta"] == 0
    assert calls["memory_bank"] == 1
    assert debug["staged_fetch"]["hard_sync_async_split"] is True
    assert debug["staged_fetch"]["sync_fallback_sources"] == [orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK]


@pytest.mark.asyncio
async def test_federated_search_policy_sources_can_keep_hard_split(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0, "letta": 0, "memory_bank": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [
            {
                "project": "alpha",
                "file": "fast/a.md",
                "summary": "fast answer",
                "score": 0.85,
                "source": "qdrant",
            }
        ]

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return []

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
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_DEEP_BLOCKING", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_MIN_FAST_RESULTS", 1)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_SLOW_REQUIRES_EXPLICIT", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_WARM_SLOW_SOURCES", False)
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
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_SLOW_SOURCES",
        [orchestrator.RETRIEVAL_SOURCE_LETTA, orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK],
    )

    _results, debug, _warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=[
            orchestrator.RETRIEVAL_SOURCE_TOPIC_ROLLUPS,
            orchestrator.RETRIEVAL_SOURCE_QDRANT,
            orchestrator.RETRIEVAL_SOURCE_MINDSDB,
            orchestrator.RETRIEVAL_SOURCE_LETTA,
            orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK,
            orchestrator.RETRIEVAL_SOURCE_MONGO_RAW,
        ],
        explicit_source_override=False,
        rerank_with_learning=False,
        retrieval_mode="fast",
    )

    assert calls["qdrant"] == 1
    assert calls["letta"] == 0
    assert debug["staged_fetch"]["hard_sync_async_split"] is True
    assert debug["staged_fetch"]["explicit_source_override"] is False


@pytest.mark.asyncio
async def test_federated_search_non_deep_sync_slow_requires_explicit(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0, "letta": 0, "memory_bank": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return []

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return []

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
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_MIN_FAST_RESULTS", 2)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_FALLBACK_SOURCES", [orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK])
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_SLOW_REQUIRES_EXPLICIT", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_WARM_SLOW_SOURCES", False)
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

    _results, debug, _warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )

    assert calls["qdrant"] == 1
    assert calls["memory_bank"] == 0
    assert calls["letta"] == 0
    assert debug["staged_fetch"]["sync_fallback_sources"] == []
    assert debug["source_policy"]["sync_slow_requires_explicit"] is True


@pytest.mark.asyncio
async def test_federated_search_circuit_blocks_degraded_slow_sources(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0, "letta": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [
            {
                "project": "alpha",
                "file": f"fast/{idx}.md",
                "summary": "fast answer",
                "score": 0.93 - (idx * 0.01),
                "source": "qdrant",
            }
            for idx in range(4)
        ]

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return []

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_STABILITY_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_CIRCUIT_SKIP_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_CIRCUIT_PROBE_EVERY_N", 99)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_STABILITY_MIN_REQUESTS", 10)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_TIMEOUT_RATE_THRESHOLD", 0.5)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_ERROR_RATE_THRESHOLD", 0.6)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_RESULTS", 10)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_TOP_SCORE", 0.98)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SLOW_SOURCE_MIN_DIVERSITY", 2)
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
    async with orchestrator.retrieval_latency_lock:
        orchestrator.retrieval_source_request_counts.clear()
        orchestrator.retrieval_source_error_counts.clear()
        orchestrator.retrieval_source_timeout_counts.clear()
        orchestrator.retrieval_slow_source_cooldown_until.clear()
        orchestrator.retrieval_slow_source_probe_attempts.clear()
        orchestrator.retrieval_source_request_counts[orchestrator.RETRIEVAL_SOURCE_LETTA] = 20
        orchestrator.retrieval_source_timeout_counts[orchestrator.RETRIEVAL_SOURCE_LETTA] = 16

    _results, debug, _warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )

    assert calls["qdrant"] == 1
    assert calls["letta"] == 0
    assert orchestrator.RETRIEVAL_SOURCE_LETTA in debug["source_policy"]["circuit_blocked_sources"]


@pytest.mark.asyncio
async def test_federated_search_backlog_gating_blocks_letta(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0, "letta": 0}

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return [{"project": "alpha", "file": "fast/a.md", "summary": "fast", "score": 0.91, "source": "qdrant"}]

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return []

    async def _empty(*args, **kwargs):
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_BACKLOG_GATING_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_BACKLOG_GATING_TARGETS", [orchestrator.RETRIEVAL_SOURCE_LETTA])
    monkeypatch.setattr(orchestrator, "RETRIEVAL_BACKLOG_GATING_LETTA_OUTSTANDING_MAX", 10)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_BACKLOG_GATING_LETTA_RETRYING_MAX", 4)
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
    orchestrator.fanout_summary_cache["by_target"] = {
        orchestrator.RETRIEVAL_SOURCE_LETTA: {"pending": 20, "retrying": 8, "running": 0}
    }
    orchestrator.fanout_summary_cache["updated_monotonic"] = time.monotonic()

    _results, debug, _warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )

    assert calls["qdrant"] == 1
    assert calls["letta"] == 0
    assert debug["source_policy"]["backlog_blocked_sources"] == [orchestrator.RETRIEVAL_SOURCE_LETTA]


@pytest.mark.asyncio
async def test_federated_search_backlog_gating_letta_async_warm(monkeypatch: pytest.MonkeyPatch):
    calls = {"qdrant": 0, "letta": 0}
    scheduled: list[str] = []

    async def _qdrant(*args, **kwargs):
        calls["qdrant"] += 1
        return []

    async def _letta(*args, **kwargs):
        calls["letta"] += 1
        return []

    async def _empty(*args, **kwargs):
        return []

    def _schedule(*, source: str, **_kwargs):
        scheduled.append(source)

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _empty)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _empty)
    monkeypatch.setattr(orchestrator, "search_topic_rollups", _empty)
    monkeypatch.setattr(orchestrator, "search_letta_archival", _letta)
    monkeypatch.setattr(orchestrator, "search_memory_bank_lexical", _empty)
    monkeypatch.setattr(orchestrator, "_schedule_background_source_warm", _schedule)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_ENABLE_STAGED_FETCH", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_PATHWAY_CACHE_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_WARM_SLOW_SOURCES", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_SLOW_REQUIRES_EXPLICIT", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_BACKLOG_GATING_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_BACKLOG_GATING_TARGETS", [orchestrator.RETRIEVAL_SOURCE_LETTA])
    monkeypatch.setattr(orchestrator, "RETRIEVAL_BACKLOG_GATING_LETTA_ASYNC_WARM_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_BACKLOG_GATING_LETTA_OUTSTANDING_MAX", 10)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_BACKLOG_GATING_LETTA_RETRYING_MAX", 4)
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
    orchestrator.fanout_summary_cache["by_target"] = {
        orchestrator.RETRIEVAL_SOURCE_LETTA: {"pending": 20, "retrying": 8, "running": 0}
    }
    orchestrator.fanout_summary_cache["updated_monotonic"] = time.monotonic()

    _results, debug, _warnings = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
    )

    assert calls["qdrant"] == 1
    assert calls["letta"] == 0
    assert orchestrator.RETRIEVAL_SOURCE_LETTA in scheduled
    assert debug["source_policy"]["backlog_async_warm_sources"] == [orchestrator.RETRIEVAL_SOURCE_LETTA]


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
    monkeypatch.setattr(orchestrator, "CONTEXTLATTICE_ENV", "production")
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


@pytest.mark.asyncio
async def test_ingest_trading_defaults_to_local_only(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "TRADING_TELEMETRY_EXTERNAL_SYNC_ENABLED", False)
    monkeypatch.setattr(orchestrator, "TRADING_TELEMETRY_EXTERNAL_SYNC_TARGETS", {"mindsdb"})
    monkeypatch.setattr(orchestrator, "MINDSDB_ENABLED", True)
    monkeypatch.setattr(orchestrator, "MINDSDB_TRADING_AUTOSYNC", True)
    mindsdb_calls = {"count": 0}

    async def _persist(_snapshot: dict[str, Any]) -> None:
        return None

    async def _push(_snapshot: dict[str, Any]) -> None:
        mindsdb_calls["count"] += 1

    monkeypatch.setattr(orchestrator, "_persist_trading_snapshot", _persist)
    monkeypatch.setattr(orchestrator, "push_trading_snapshot_to_mindsdb", _push)
    payload = orchestrator.TradingMetrics(
        timestamp=datetime.now(timezone.utc),
        open_positions=1,
        total_value_usd=1000.0,
        unrealized_pnl=10.0,
        realized_pnl=5.0,
        daily_pnl=2.0,
        positions=[],
        price_cache_entries=1,
        price_cache_max_age=1.0,
        price_cache_ttl=10.0,
        price_cache_freshness=0.9,
        price_cache_penalty=1.0,
    )

    result = await orchestrator.ingest_trading(payload)
    assert result["ok"] is True
    assert result["mindsdb_synced"] is False
    assert result["warning"] is None
    assert mindsdb_calls["count"] == 0
    assert result["external_sync"]["enabled"] is False
    assert result["external_sync"]["mindsdb"] == "disabled"


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
async def test_memory_search_deep_async_returns_poll_token(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, Any] = {}

    async def _enqueue(*, payload: Any, callback_url: str | None):
        captured["payload"] = payload
        captured["callback_url"] = callback_url
        return {
            "ok": True,
            "async": True,
            "token": "tok-1",
            "status": "queued",
            "poll_url": "/memory/search/async/tok-1",
        }

    monkeypatch.setattr(orchestrator, "RECALL_DEEP_ASYNC_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RECALL_DEEP_ASYNC_PARTIAL_ENABLED", False)
    monkeypatch.setattr(orchestrator, "_enqueue_recall_deep_async_job", _enqueue)
    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="deep search",
            retrieval_mode="deep",
            deep_async=True,
        )
    )
    assert response["async"] is True
    assert response["token"] == "tok-1"
    assert response["job_id"] == "tok-1"
    assert captured["callback_url"] is None


@pytest.mark.asyncio
async def test_memory_search_deep_async_requires_deep_mode(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RECALL_DEEP_ASYNC_ENABLED", True)
    with pytest.raises(orchestrator.HTTPException) as exc:
        await orchestrator.search_memory(
            orchestrator.MemorySearch(
                query="not deep",
                retrieval_mode="balanced",
                deep_async=True,
            )
        )
    assert exc.value.status_code == 422


@pytest.mark.asyncio
async def test_memory_search_deep_async_returns_partial_plus_job(monkeypatch: pytest.MonkeyPatch):
    async def _enqueue(*, payload: Any, callback_url: str | None):
        return {
            "ok": True,
            "async": True,
            "token": "tok-partial",
            "job_id": "tok-partial",
            "status": "queued",
            "poll_url": "/memory/search/async/tok-partial",
            "job_poll_url": "/memory/search/jobs/tok-partial",
            "expires_at": "2026-03-14T00:00:00Z",
        }

    async def _partial(payload: Any):
        return (
            {
                "results": [
                    {
                        "project": "alpha",
                        "file": "notes/quick.md",
                        "summary": "quick partial",
                        "score": 0.81,
                        "source": "topic_rollups",
                    }
                ],
                "warnings": ["Sources returned now: topic_rollups."],
                "retrieval_mode": "fast",
                "retrieval_intent": "decision",
                "degraded": False,
            },
            "fast",
        )

    monkeypatch.setattr(orchestrator, "RECALL_DEEP_ASYNC_ENABLED", True)
    monkeypatch.setattr(orchestrator, "_enqueue_recall_deep_async_job", _enqueue)
    monkeypatch.setattr(orchestrator, "_build_recall_deep_async_partial_response", _partial)
    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="deep partial",
            retrieval_mode="deep",
            deep_async=True,
        )
    )
    assert response["async"] is True
    assert response["partial"] is True
    assert response["token"] == "tok-partial"
    assert response["job_id"] == "tok-partial"
    assert response["job_poll_url"] == "/memory/search/jobs/tok-partial"
    assert response["continuation_async"]["token"] == "tok-partial"
    assert response["continuation_async"]["events_url"] == "/memory/search/continuations/tok-partial/events"
    assert response["continuation_async"]["legacy_events_url"] == "/memory/search/jobs/tok-partial/events"
    assert response["results"][0]["source"] == "topic_rollups"
    assert response["retrieval_lifecycle"]["status"] == "queued"
    assert response["retrieval_lifecycle"]["partial"] is True
    assert any("Deep retrieval is running asynchronously" in warning for warning in response["warnings"])


@pytest.mark.asyncio
async def test_memory_search_deep_async_default_for_deep(monkeypatch: pytest.MonkeyPatch):
    async def _enqueue(*, payload: Any, callback_url: str | None):
        return {
            "ok": True,
            "async": True,
            "token": "tok-auto",
            "job_id": "tok-auto",
            "status": "queued",
            "poll_url": "/memory/search/async/tok-auto",
            "job_poll_url": "/memory/search/jobs/tok-auto",
        }

    monkeypatch.setattr(orchestrator, "RECALL_DEEP_ASYNC_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RECALL_DEEP_ASYNC_DEFAULT_FOR_DEEP", True)
    monkeypatch.setattr(orchestrator, "RECALL_DEEP_ASYNC_PARTIAL_ENABLED", False)
    monkeypatch.setattr(orchestrator, "_enqueue_recall_deep_async_job", _enqueue)
    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="auto deep async",
            retrieval_mode="deep",
            deep_async=None,
        )
    )
    assert response["async"] is True
    assert response["deep_async_auto"] is True
    assert response["token"] == "tok-auto"


@pytest.mark.asyncio
async def test_get_memory_search_async_returns_completed_result(monkeypatch: pytest.MonkeyPatch):
    token = "tok-status"
    expires = time.monotonic() + 120.0
    async with orchestrator.recall_deep_async_lock:
        orchestrator.recall_deep_async_jobs.clear()
        orchestrator.recall_deep_async_jobs[token] = {
            "token": token,
            "status": "completed",
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:01Z",
            "completed_at": "2026-03-12T00:00:01Z",
            "expires_monotonic": expires,
            "expires_at": orchestrator._recall_deep_async_expires_at_iso(expires),
            "result": {"results": [{"project": "alpha"}]},
            "error": None,
        }
    response = await orchestrator.get_memory_search_async(token)
    assert response["status"] == "completed"
    assert response["job_id"] == token
    assert response["job_poll_url"] == f"/memory/search/jobs/{token}"
    assert response["events_url"] == f"/memory/search/jobs/{token}/events"
    assert response["continuation_async"]["poll_url"] == f"/memory/search/continuations/{token}"
    assert response["continuation_async"]["events_url"] == f"/memory/search/continuations/{token}/events"
    assert response["result"]["results"][0]["project"] == "alpha"


@pytest.mark.asyncio
async def test_get_memory_search_job_alias_returns_completed_result(monkeypatch: pytest.MonkeyPatch):
    token = "tok-status-alias"
    expires = time.monotonic() + 120.0
    async with orchestrator.recall_deep_async_lock:
        orchestrator.recall_deep_async_jobs.clear()
        orchestrator.recall_deep_async_jobs[token] = {
            "token": token,
            "status": "completed",
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:01Z",
            "completed_at": "2026-03-12T00:00:01Z",
            "expires_monotonic": expires,
            "expires_at": orchestrator._recall_deep_async_expires_at_iso(expires),
            "result": {"results": [{"project": "alpha"}]},
            "error": None,
        }
    response = await orchestrator.get_memory_search_job(token)
    assert response["status"] == "completed"
    assert response["job_id"] == token


@pytest.mark.asyncio
async def test_get_memory_search_continuation_alias_returns_completed_result(monkeypatch: pytest.MonkeyPatch):
    token = "tok-status-continuation"
    expires = time.monotonic() + 120.0
    async with orchestrator.recall_deep_async_lock:
        orchestrator.recall_deep_async_jobs.clear()
        orchestrator.recall_deep_async_jobs[token] = {
            "token": token,
            "status": "completed",
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:01Z",
            "completed_at": "2026-03-12T00:00:01Z",
            "expires_monotonic": expires,
            "expires_at": orchestrator._recall_deep_async_expires_at_iso(expires),
            "result": {"results": [{"project": "alpha"}]},
            "error": None,
        }
    response = await orchestrator.get_memory_search_continuation(token)
    assert response["status"] == "completed"
    assert response["job_id"] == token
    assert response["continuation_async"]["events_url"] == f"/memory/search/continuations/{token}/events"


@pytest.mark.asyncio
async def test_stream_memory_search_job_events_returns_snapshot_for_completed_job():
    token = "tok-events"
    expires = time.monotonic() + 120.0
    async with orchestrator.recall_deep_async_lock:
        orchestrator.recall_deep_async_jobs.clear()
        orchestrator.recall_deep_async_jobs[token] = {
            "token": token,
            "status": "completed",
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:01Z",
            "completed_at": "2026-03-12T00:00:01Z",
            "expires_monotonic": expires,
            "expires_at": orchestrator._recall_deep_async_expires_at_iso(expires),
            "result": {"results": [{"project": "alpha"}]},
            "error": None,
        }
    response = await orchestrator.stream_memory_search_job_events(token)
    assert response.media_type == "text/event-stream"
    chunks: list[bytes] = []
    async for chunk in response.body_iterator:
        if isinstance(chunk, str):
            chunks.append(chunk.encode("utf-8"))
        else:
            chunks.append(bytes(chunk))
    payload = b"".join(chunks).decode("utf-8")
    assert "event: snapshot" in payload
    assert "\"status\":\"completed\"" in payload


@pytest.mark.asyncio
async def test_stream_memory_search_continuation_events_alias_returns_snapshot_for_completed_job():
    token = "tok-events-continuation"
    expires = time.monotonic() + 120.0
    async with orchestrator.recall_deep_async_lock:
        orchestrator.recall_deep_async_jobs.clear()
        orchestrator.recall_deep_async_jobs[token] = {
            "token": token,
            "status": "completed",
            "created_at": "2026-03-12T00:00:00Z",
            "updated_at": "2026-03-12T00:00:01Z",
            "completed_at": "2026-03-12T00:00:01Z",
            "expires_monotonic": expires,
            "expires_at": orchestrator._recall_deep_async_expires_at_iso(expires),
            "result": {"results": [{"project": "alpha"}]},
            "error": None,
        }
    response = await orchestrator.stream_memory_search_continuation_events(token)
    assert response.media_type == "text/event-stream"
    chunks: list[bytes] = []
    async for chunk in response.body_iterator:
        if isinstance(chunk, str):
            chunks.append(chunk.encode("utf-8"))
        else:
            chunks.append(bytes(chunk))
    payload = b"".join(chunks).decode("utf-8")
    assert "event: snapshot" in payload
    assert "\"status\":\"completed\"" in payload


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
async def test_memory_search_resolves_project_alias(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, Any] = {}

    async def _pipeline(**kwargs):
        captured.update(kwargs)
        return (
            [
                {
                    "project": "algotraderv2_rust",
                    "file": "decisions/alpha.md",
                    "summary": "decision note",
                    "score": 0.8,
                    "topic_path": "decisions",
                }
            ],
            {"retrieval_mode": "balanced", "source_errors": {}, "source_counts": {}},
            [],
            {"strict_numeric_copy": True, "facts": [], "numeric_facts": []},
        )

    async def _projects():
        return ["algotraderv2_rust", "alpha"]

    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", False)
    monkeypatch.setattr(orchestrator, "PROJECT_ALIAS_MAP", {"algotrader": "algotraderv2_rust"})
    monkeypatch.setattr(orchestrator, "list_projects", _projects)
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)

    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="decision",
            project="algotrader",
            include_retrieval_debug=True,
        )
    )

    assert captured["project_filter"] == "algotraderv2_rust"
    assert response["project_resolution"]["aliasApplied"] is True
    assert response["project_resolution"]["resolved"] == "algotraderv2_rust"


@pytest.mark.asyncio
async def test_memory_search_suggests_similar_project_when_missing(monkeypatch: pytest.MonkeyPatch):
    called = {"pipeline": 0}

    async def _pipeline(**kwargs):
        called["pipeline"] += 1
        return (
            [],
            {"retrieval_mode": "balanced", "source_errors": {}, "source_counts": {}},
            [],
            {"strict_numeric_copy": True, "facts": [], "numeric_facts": []},
        )

    async def _projects():
        return ["algotraderv2_rust", "alpha"]

    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", False)
    monkeypatch.setattr(orchestrator, "PROJECT_ALIAS_MAP", {})
    monkeypatch.setattr(orchestrator, "PROJECT_NAME_FAIL_FAST_ON_MISS", True)
    monkeypatch.setattr(orchestrator, "list_projects", _projects)
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)

    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="pnl",
            project="algotrader",
            include_retrieval_debug=True,
            include_grounding=True,
        )
    )

    assert response["results"] == []
    assert called["pipeline"] == 0
    assert "algotraderv2_rust" in response["project_suggestions"]
    assert response["project_resolution"]["verified"] is False
    assert response["retrieval"]["topic_scope"]["total_results"] == 0


@pytest.mark.asyncio
async def test_memory_search_applies_trading_scope_filter(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, Any] = {}

    async def _pipeline(**kwargs):
        captured.update(kwargs)
        return (
            [
                {
                    "project": "algotraderv2_rust",
                    "file": "telemetry/queue__latest.json",
                    "summary": "queue snapshot",
                    "score": 0.96,
                    "topic_path": "telemetry",
                },
                {
                    "project": "algotraderv2_rust",
                    "file": "profitability/weekly.md",
                    "summary": "weekly profitability summary",
                    "score": 0.62,
                    "topic_path": "profitability",
                },
            ],
            {"retrieval_mode": "balanced", "source_errors": {}, "source_counts": {}},
            [],
            {"strict_numeric_copy": True, "facts": [], "numeric_facts": []},
        )

    async def _projects():
        return ["algotraderv2_rust", "alpha"]

    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", False)
    monkeypatch.setattr(orchestrator, "TRADING_PROJECT_HINTS", ["algotraderv2_rust"])
    monkeypatch.setattr(orchestrator, "TRADING_DEFAULT_TOPIC_PREFIXES", ["decisions", "reports", "profitability"])
    monkeypatch.setattr(orchestrator, "list_projects", _projects)
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)

    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="profitability",
            project="algotraderv2_rust",
            include_retrieval_debug=True,
        )
    )

    assert captured["topic_filter"] is None
    assert len(response["results"]) == 1
    assert response["results"][0]["topic_path"] == "profitability"
    assert response["retrieval"]["topic_scope"]["applied"] is True
    assert response["retrieval"]["topic_scope"]["matched_results"] == 1


@pytest.mark.asyncio
async def test_memory_search_applies_trading_project_intent_sources(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, Any] = {}

    async def _pipeline(**kwargs):
        captured.update(kwargs)
        return (
            [],
            {"retrieval_mode": "balanced", "retrieval_intent": "decision", "source_errors": {}, "source_counts": {}},
            [],
            {"strict_numeric_copy": True, "facts": [], "numeric_facts": []},
        )

    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", False)
    monkeypatch.setattr(orchestrator, "TRADING_PROJECT_HINTS", ["algotraderv2_rust"])
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)

    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="profitability tuning",
            project="algotraderv2_rust",
            include_retrieval_debug=True,
        )
    )

    assert captured["sources"] is None
    assert captured["agent_profile"]["sources"] == orchestrator._resolve_intent_default_sources("decision")
    assert captured["agent_profile"]["_source_override_requested"] is False
    assert response["retrieval_policy"]["projectDefaultsApplied"] is True
    assert response["retrieval_policy"]["intentDefaultSourcesApplied"] is True
    assert response["retrieval_policy"]["sourceOverrideRequested"] is False
    assert response["retrieval_policy"]["effectiveSources"] == orchestrator._resolve_intent_default_sources("decision")


def test_normalize_retrieval_sources_respects_memory_bank_default_policy(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_BANK_DEFAULT_ENABLED", False)
    monkeypatch.setattr(
        orchestrator,
        "RETRIEVAL_SOURCES_ENV",
        ",".join(
            [
                orchestrator.RETRIEVAL_SOURCE_QDRANT,
                orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK,
                orchestrator.RETRIEVAL_SOURCE_LETTA,
            ]
        ),
    )
    default_sources = orchestrator._normalize_retrieval_sources(
        None,
        explicit_source_override=False,
    )
    explicit_sources = orchestrator._normalize_retrieval_sources(
        [
            orchestrator.RETRIEVAL_SOURCE_QDRANT,
            orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK,
        ],
        explicit_source_override=True,
    )
    assert orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK not in default_sources
    assert orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK in explicit_sources


@pytest.mark.asyncio
async def test_run_memory_recall_pipeline_filters_profile_memory_bank_when_default_off(
    monkeypatch: pytest.MonkeyPatch,
):
    captured: dict[str, Any] = {}

    async def _federated(query: str, **kwargs):
        captured["query"] = query
        captured["sources"] = list(kwargs.get("sources") or [])
        captured["explicit"] = bool(kwargs.get("explicit_source_override"))
        return [], {"sources": list(kwargs.get("sources") or []), "source_errors": {}, "source_counts": {}}, []

    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_BANK_DEFAULT_ENABLED", False)
    monkeypatch.setattr(orchestrator, "federated_search_memory", _federated)
    monkeypatch.setattr(orchestrator, "AGENT_RECALL_ESCALATION_ENABLED", False)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_QUERY_EXPANSION_ENABLED", False)

    _results, debug, _warnings, _grounding = await orchestrator._run_memory_recall_pipeline(
        query="latency profile",
        limit=5,
        project_filter="alpha",
        topic_filter=None,
        sources=None,
        source_weights=None,
        preferences=None,
        rerank_with_learning=False,
        retrieval_mode="balanced",
        retrieval_intent="decision",
        agent_profile={
            "sources": [
                orchestrator.RETRIEVAL_SOURCE_QDRANT,
                orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK,
            ],
            "_source_override_requested": False,
        },
        auto_escalate=False,
        query_expansion=False,
    )

    assert captured["explicit"] is False
    assert orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK not in captured["sources"]
    assert orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK not in (debug.get("sources") or [])


@pytest.mark.asyncio
async def test_memory_search_effective_sources_filter_memory_bank_when_default_off(
    monkeypatch: pytest.MonkeyPatch,
):
    captured: dict[str, Any] = {}

    async def _pipeline(**kwargs):
        captured.update(kwargs)
        return (
            [],
            {"retrieval_mode": "balanced", "retrieval_intent": "decision", "source_errors": {}, "source_counts": {}},
            [],
            {"strict_numeric_copy": True, "facts": [], "numeric_facts": []},
        )

    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", False)
    monkeypatch.setattr(orchestrator, "TRADING_PROJECT_HINTS", ["algotraderv2_rust"])
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_BANK_DEFAULT_ENABLED", False)
    monkeypatch.setattr(
        orchestrator,
        "DEFAULT_RETRIEVAL_INTENT_SOURCES",
        {
            "decision": [
                orchestrator.RETRIEVAL_SOURCE_TOPIC_ROLLUPS,
                orchestrator.RETRIEVAL_SOURCE_QDRANT,
                orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK,
            ],
            "ops": [orchestrator.RETRIEVAL_SOURCE_QDRANT],
            "raw": [orchestrator.RETRIEVAL_SOURCE_MONGO_RAW],
        },
    )
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)

    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="profitability tuning",
            project="algotraderv2_rust",
            include_retrieval_debug=True,
        )
    )

    assert orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK not in (
        captured["agent_profile"].get("sources") or []
    )
    assert orchestrator.RETRIEVAL_SOURCE_MEMORY_BANK not in (
        response["retrieval_policy"].get("effectiveSources") or []
    )


@pytest.mark.asyncio
async def test_memory_search_raw_intent_skips_trading_scope(monkeypatch: pytest.MonkeyPatch):
    async def _pipeline(**_kwargs):
        return (
            [
                {
                    "project": "algotraderv2_rust",
                    "file": "telemetry/queue__latest.json",
                    "summary": "queue snapshot",
                    "score": 0.9,
                    "topic_path": "telemetry",
                }
            ],
            {"retrieval_mode": "balanced", "retrieval_intent": "raw", "source_errors": {}, "source_counts": {}},
            [],
            {"strict_numeric_copy": True, "facts": [], "numeric_facts": []},
        )

    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", False)
    monkeypatch.setattr(orchestrator, "TRADING_PROJECT_HINTS", ["algotraderv2_rust"])
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)

    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="exit manager state",
            project="algotraderv2_rust",
            retrieval_intent="raw",
            include_retrieval_debug=True,
        )
    )

    assert response["retrieval"]["topic_scope"]["applied"] is False


@pytest.mark.asyncio
async def test_memory_search_includes_retrieval_lifecycle_pending_sources(monkeypatch: pytest.MonkeyPatch):
    async def _pipeline(**_kwargs):
        return (
            [],
            {
                "retrieval_mode": "deep",
                "retrieval_intent": "decision",
                "source_errors": {},
                "source_counts": {"topic_rollups": 0},
                "staged_fetch": {"async_warm_sources": ["letta"]},
            },
            ["slow sources still warming"],
            {"strict_numeric_copy": True, "facts": [], "numeric_facts": []},
        )

    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", False)
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)

    response = await orchestrator.search_memory(
        orchestrator.MemorySearch(
            query="deep recall",
            retrieval_mode="deep",
        )
    )

    assert response["result_state"] == "pending"
    assert response["retrieval_lifecycle"]["status"] == "partial"
    assert response["retrieval_lifecycle"]["sources"]["pending"] == ["letta"]
    assert "retry_after_cache_warm" in response["retrieval_lifecycle"]["next_actions"]


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
async def test_context_pack_rollup_includes_raw_refs(monkeypatch: pytest.MonkeyPatch):
    async def _search(_: Any):
        return {
            "results": [
                {
                    "project": "alpha",
                    "file": "_rollups/topics/decisions.json",
                    "summary": "Rollup summary",
                    "score": 0.81,
                    "source": orchestrator.RETRIEVAL_SOURCE_TOPIC_ROLLUPS,
                    "topic_path": "decisions",
                    "topic_rollup": {
                        "event_count": 20,
                        "recent_event_count": 5,
                        "unique_file_count": 3,
                        "latest_timestamp": "2026-03-02T10:00:00Z",
                        "raw_refs": ["decisions/a.md", "decisions/b.md"],
                        "file_partitions": [
                            {
                                "topic_path": "decisions",
                                "file_count": 3,
                                "sample_files": ["decisions/a.md", "decisions/b.md"],
                            }
                        ],
                    },
                }
            ],
            "grounding": {
                "strict_numeric_copy": True,
                "facts": [],
                "numeric_facts": [],
            },
            "warnings": [],
            "retrieval_mode": "balanced",
            "retrieval_intent": "decision",
            "agent_id": "default",
        }

    monkeypatch.setattr(orchestrator, "search_memory", _search)
    response = await orchestrator.get_memory_context_pack(
        orchestrator.ContextPackRequest(query="decision rollup", limit=5, max_facts=5)
    )
    pack = response["context_pack"]
    assert pack["results"][0]["topic_rollup"]["raw_refs"] == ["decisions/a.md", "decisions/b.md"]
    assert pack["results"][0]["topic_rollup"]["file_partitions"][0]["topic_path"] == "decisions"
    assert any(
        citation.get("source") == "topic_rollup_raw_ref" and citation.get("file") == "decisions/a.md"
        for citation in pack["citations"]
    )
    assert any(
        citation.get("source") == "topic_rollup_partition_ref" and citation.get("file") == "decisions/b.md"
        for citation in pack["citations"]
    )


def test_finalize_topic_rollup_entry_builds_file_partitions(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "TOPIC_ROLLUP_FILE_PARTITIONS_MAX", 3)
    entry = orchestrator._new_topic_rollup_entry("runbooks/performance")
    entry["_unique_files"] = {
        "runbooks/performance/a.md",
        "runbooks/performance/b.md",
        "runbooks/profitability/c.md",
        "telemetry/stats/d.json",
    }
    finalized = orchestrator._finalize_topic_rollup_entry(entry)
    partitions = finalized.get("filePartitions")
    assert isinstance(partitions, list)
    assert partitions
    assert sum(int(item.get("file_count") or 0) for item in partitions) == finalized["uniqueFileCount"]


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


@pytest.mark.asyncio
async def test_evaluate_memory_recall_retries_transient_failures(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RECALL_EVAL_TRANSIENT_RETRY_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RECALL_EVAL_TRANSIENT_RETRY_MAX_ATTEMPTS", 1)
    monkeypatch.setattr(orchestrator, "RECALL_EVAL_TRANSIENT_RETRY_MODES_ENV", "balanced")
    calls: list[str | None] = []

    async def _search(payload: Any):
        calls.append(payload.retrieval_mode)
        if len(calls) == 1:
            return {
                "results": [],
                "warnings": ["qdrant retrieval failed: qdrant retrieval timed out after 5.2s"],
                "grounding": {"numeric_facts": []},
                "retrieval_mode": "fast",
                "agent_id": "default",
                "retrieval": {"source_errors": {"qdrant": "timeout"}},
            }
        return {
            "results": [
                {
                    "project": "alpha",
                    "file": "notes/health.md",
                    "summary": "health status improved after tuning",
                    "source": "topic_rollups",
                    "score": 0.81,
                }
            ],
            "warnings": [],
            "grounding": {"numeric_facts": []},
            "retrieval_mode": "balanced",
            "agent_id": "default",
            "retrieval": {"source_errors": {}},
        }

    monkeypatch.setattr(orchestrator, "search_memory", _search)
    payload = orchestrator.RecallEvalRequest(
        cases=[
            orchestrator.RecallEvalCase(
                id="case-transient",
                query="health status",
                expected_substrings=["health"],
                retrieval_mode="fast",
            )
        ],
        k=5,
    )
    result = await orchestrator.evaluate_memory_recall(payload)
    assert result["passed"] is True
    assert calls == ["fast", "balanced"]
    case = result["cases"][0]
    assert case["hit"] is True
    assert case["retry_attempts"] == 1
    assert case["transient_retry_triggered"] is True
    assert case["transient_retry_recovered"] is True
    assert case["attempt_modes"] == ["fast", "balanced"]


@pytest.mark.asyncio
async def test_evaluate_memory_recall_no_retry_without_transient_signal(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RECALL_EVAL_TRANSIENT_RETRY_ENABLED", True)
    monkeypatch.setattr(orchestrator, "RECALL_EVAL_TRANSIENT_RETRY_MAX_ATTEMPTS", 1)
    monkeypatch.setattr(orchestrator, "RECALL_EVAL_TRANSIENT_RETRY_MODES_ENV", "balanced")
    calls: list[str | None] = []

    async def _search(payload: Any):
        calls.append(payload.retrieval_mode)
        return {
            "results": [],
            "warnings": ["no qualifying retrieval hits"],
            "grounding": {"numeric_facts": []},
            "retrieval_mode": "fast",
            "agent_id": "default",
            "retrieval": {"source_errors": {}},
        }

    monkeypatch.setattr(orchestrator, "search_memory", _search)
    payload = orchestrator.RecallEvalRequest(
        cases=[
            orchestrator.RecallEvalCase(
                id="case-non-transient",
                query="health status",
                expected_substrings=["health"],
                retrieval_mode="fast",
            )
        ],
        k=5,
    )
    result = await orchestrator.evaluate_memory_recall(payload)
    assert result["passed"] is False
    assert calls == ["fast"]
    case = result["cases"][0]
    assert case["retry_attempts"] == 0
    assert case["transient_retry_triggered"] is False
    assert case["transient_retry_recovered"] is False
    assert case["attempt_modes"] == ["fast"]


@pytest.mark.asyncio
async def test_refresh_saved_recall_eval_cases_persists_and_runs_eval(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
):
    case_path = tmp_path / "recall_eval_cases.json"
    monkeypatch.setattr(orchestrator, "RECALL_EVAL_CASES_PATH", case_path)

    async def _build_cases(**kwargs):
        assert kwargs["max_cases"] == 3
        assert kwargs["min_hits"] == 1
        return {
            "version": 1,
            "updatedAt": "2026-03-12T00:00:00Z",
            "k": 5,
            "gate": {"minRecallAtK": 0.75, "minMrr": 0.55, "minNumericExactness": 0.9},
            "cases": [
                {
                    "id": "c1",
                    "query": "source quality",
                    "project": "algotraderv2_rust",
                    "topic_path": "runbooks/performance",
                    "expected_substrings": ["source"],
                }
            ],
        }

    async def _eval_saved(payload):
        assert payload.include_preferences is True
        return {"ok": True, "passed": True, "metrics": {"recallAtK": 1.0}, "gate": {}, "cases": []}

    monkeypatch.setattr(orchestrator, "_build_refreshed_recall_eval_case_set", _build_cases)
    monkeypatch.setattr(orchestrator, "evaluate_saved_recall_cases", _eval_saved)
    payload = orchestrator.RecallEvalRefreshRequest(max_cases=3, min_hits=1, run_evaluation=True)
    result = await orchestrator.refresh_saved_recall_eval_cases(payload)
    assert result["ok"] is True
    assert result["savedCaseSet"]["count"] == 1
    assert result["evaluation"]["passed"] is True
    persisted = json.loads(case_path.read_text(encoding="utf-8"))
    assert persisted["cases"][0]["id"] == "c1"


@pytest.mark.asyncio
async def test_build_refreshed_recall_eval_case_set_uses_hot_pathways(monkeypatch: pytest.MonkeyPatch):
    async def _hot(_limit: int):
        return [
            {
                "query": "source quality qdrant",
                "project_filter": "algotraderv2_rust",
                "topic_filter": "runbooks/performance",
                "sources": ["qdrant", "topic_rollups"],
                "source_weights": {"qdrant": 1.0},
                "retrieval_mode": "deep",
                "retrieval_intent": "decision",
                "hits": 5,
            }
        ]

    monkeypatch.setattr(orchestrator, "_list_hot_retrieval_pathways", _hot)
    async def _pipeline(**kwargs):
        return (
            [
                {
                    "project": "algotraderv2_rust",
                    "file": "notes/perf.md",
                    "summary": "qdrant source quality improved",
                    "source": "qdrant",
                    "score": 0.9,
                }
            ],
            {},
            [],
            {},
        )

    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)
    payload = await orchestrator._build_refreshed_recall_eval_case_set(
        max_cases=5,
        min_hits=2,
        project="algotraderv2_rust",
        topic_prefix="runbooks",
    )
    assert len(payload["cases"]) == 1
    case = payload["cases"][0]
    assert case["project"] == "algotraderv2_rust"
    assert case["topic_path"] == "runbooks/performance"
    assert case["retrieval_mode"] == "fast"
    assert case["sources"] == ["qdrant", "topic_rollups"]
    assert "qdrant" in (case.get("expected_substrings") or [])


@pytest.mark.asyncio
async def test_build_refreshed_recall_eval_case_set_prunes_fallback_non_hits(monkeypatch: pytest.MonkeyPatch):
    async def _hot(_limit: int):
        return []

    async def _pipeline(**kwargs):
        query = str(kwargs.get("query") or "")
        if "runtime cutover" in query:
            return ([], {}, [], {})
        return (
            [
                {
                    "project": "algotraderv2_rust",
                    "file": "notes/perf.md",
                    "summary": "qdrant source quality improved",
                    "source": "qdrant",
                    "score": 0.9,
                }
            ],
            {},
            [],
            {},
        )

    monkeypatch.setattr(orchestrator, "_list_hot_retrieval_pathways", _hot)
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)
    payload = await orchestrator._build_refreshed_recall_eval_case_set(
        max_cases=5,
        min_hits=1,
        project="algotraderv2_rust",
        topic_prefix=None,
    )
    assert len(payload["cases"]) == 2
    ids = {case.get("id") for case in payload["cases"]}
    assert "runtime-cutover-surface" not in ids


@pytest.mark.asyncio
async def test_build_refreshed_recall_eval_case_set_requires_rollup_support(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RECALL_EVAL_REFRESH_REQUIRE_ROLLUP_SUPPORT", True)

    async def _hot(_limit: int):
        return [
            {
                "query": "source quality qdrant",
                "project_filter": "algotraderv2_rust",
                "topic_filter": "runbooks/performance",
                "retrieval_intent": "decision",
                "hits": 5,
            }
        ]

    async def _pipeline(**kwargs):
        sources = kwargs.get("sources") or []
        if sources == [orchestrator.RETRIEVAL_SOURCE_TOPIC_ROLLUPS]:
            return ([], {}, [], {})
        return (
            [
                {
                    "project": "algotraderv2_rust",
                    "file": "notes/perf.md",
                    "summary": "qdrant source quality improved",
                    "source": "qdrant",
                    "score": 0.9,
                }
            ],
            {},
            [],
            {},
        )

    monkeypatch.setattr(orchestrator, "_list_hot_retrieval_pathways", _hot)
    monkeypatch.setattr(orchestrator, "_run_memory_recall_pipeline", _pipeline)
    payload = await orchestrator._build_refreshed_recall_eval_case_set(
        max_cases=5,
        min_hits=1,
        project="algotraderv2_rust",
        topic_prefix=None,
    )
    assert payload["cases"] == []


def test_fastembed_adapter_enabled_honors_gate(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "FASTEMBED_RS_GATE_REQUIRED", True)
    monkeypatch.setattr(orchestrator, "_fastembed_adapter_enabled_by_flag", lambda: True)
    monkeypatch.setattr(orchestrator, "_fastembed_gate_status", lambda: {"passed": False})
    assert orchestrator._fastembed_adapter_enabled() is False


def test_fastembed_adapter_enabled_without_gate_requirement(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "FASTEMBED_RS_GATE_REQUIRED", False)
    monkeypatch.setattr(orchestrator, "_fastembed_adapter_enabled_by_flag", lambda: True)
    monkeypatch.setattr(orchestrator, "_fastembed_gate_status", lambda: {"passed": False})
    assert orchestrator._fastembed_adapter_enabled() is True


def test_fastembed_adapter_enabled_with_effective_override(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "FASTEMBED_RS_GATE_REQUIRED", True)
    monkeypatch.setattr(orchestrator, "_fastembed_adapter_enabled_by_flag", lambda: True)
    monkeypatch.setattr(
        orchestrator,
        "_fastembed_gate_status",
        lambda: {"passed": False, "effectivePassed": True},
    )
    assert orchestrator._fastembed_adapter_enabled() is True


def test_apply_fastembed_promote_override_marks_effective_pass(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "FASTEMBED_RS_PROMOTE_OVERRIDE", True)
    monkeypatch.setattr(orchestrator, "FASTEMBED_RS_PROMOTE_REASON", "manual_16pct_promotion")
    status = orchestrator._apply_fastembed_promote_override(
        {
            "required": True,
            "passed": False,
            "reason": "threshold_not_met",
        }
    )
    assert status["passed"] is False
    assert status["effectivePassed"] is True
    assert status["promoteOverrideActive"] is True
    assert "manual_16pct_promotion" in str(status["reason"])


@pytest.mark.asyncio
async def test_embed_text_prefers_fastembed_adapter(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "_fastembed_adapter_ready", lambda: True)

    async def _fastembed(text: str):
        assert text == "alpha beta"
        return [0.25, 0.75]

    async def _cache_get(_key: str):
        return None

    captured: dict[str, Any] = {}

    async def _cache_set(_key: str, vector: list[float]):
        captured["vector"] = vector

    monkeypatch.setattr(orchestrator, "_fastembed_rs_embedding", _fastembed)
    monkeypatch.setattr(orchestrator, "_embedding_cache_get", _cache_get)
    monkeypatch.setattr(orchestrator, "_embedding_cache_set", _cache_set)
    vector = await orchestrator.embed_text("alpha beta")
    assert vector == [0.25, 0.75]
    assert captured["vector"] == [0.25, 0.75]


@pytest.mark.asyncio
async def test_embed_text_fastembed_fallbacks_to_provider(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "_fastembed_adapter_ready", lambda: True)
    monkeypatch.setattr(orchestrator, "EMBEDDING_PROVIDER", "openai")
    monkeypatch.setattr(orchestrator, "fastembed_adapter_fallbacks", 0)

    async def _fastembed(_text: str):
        raise RuntimeError("adapter unavailable")

    async def _cache_get(_key: str):
        return None

    async def _cache_set(_key: str, _vector: list[float]):
        return None

    async def _openai(_text: str):
        return [0.1, 0.2, 0.7]

    monkeypatch.setattr(orchestrator, "_fastembed_rs_embedding", _fastembed)
    monkeypatch.setattr(orchestrator, "_embedding_cache_get", _cache_get)
    monkeypatch.setattr(orchestrator, "_embedding_cache_set", _cache_set)
    monkeypatch.setattr(orchestrator, "_openai_like_embedding", _openai)
    vector = await orchestrator.embed_text("fallback query")
    assert vector == [0.1, 0.2, 0.7]
    assert orchestrator.fastembed_adapter_fallbacks == 1


@pytest.mark.asyncio
async def test_embed_text_batch_prefers_fastembed_adapter(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "_fastembed_adapter_ready", lambda: True)

    captured: dict[str, Any] = {}

    async def _fastembed_batch(texts: list[str]):
        captured["texts"] = list(texts)
        return [[float(idx), float(idx) + 0.5] for idx, _ in enumerate(texts, start=1)]

    async def _cache_get(_key: str):
        return None

    cache_writes: list[list[float]] = []

    async def _cache_set(_key: str, vector: list[float]):
        cache_writes.append(list(vector))

    monkeypatch.setattr(orchestrator, "_fastembed_rs_embedding_batch", _fastembed_batch)
    monkeypatch.setattr(orchestrator, "_embedding_cache_get", _cache_get)
    monkeypatch.setattr(orchestrator, "_embedding_cache_set", _cache_set)
    rows = await orchestrator.embed_text_batch(["alpha", "beta", "alpha"])
    assert captured["texts"] == ["alpha", "beta"]
    assert rows == [[1.0, 1.5], [2.0, 2.5], [1.0, 1.5]]
    assert len(cache_writes) == 3


@pytest.mark.asyncio
async def test_embed_text_batch_fastembed_fallbacks_to_provider_batch(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "_fastembed_adapter_ready", lambda: True)
    monkeypatch.setattr(orchestrator, "EMBEDDING_PROVIDER", "openai")
    monkeypatch.setattr(orchestrator, "fastembed_adapter_fallbacks", 0)

    async def _fastembed_batch(_texts: list[str]):
        raise RuntimeError("adapter unavailable")

    async def _openai_batch(texts: list[str]):
        return [[float(len(text)), 0.0] for text in texts]

    async def _cache_get(_key: str):
        return None

    async def _cache_set(_key: str, _vector: list[float]):
        return None

    monkeypatch.setattr(orchestrator, "_fastembed_rs_embedding_batch", _fastembed_batch)
    monkeypatch.setattr(orchestrator, "_openai_like_embedding_batch", _openai_batch)
    monkeypatch.setattr(orchestrator, "_embedding_cache_get", _cache_get)
    monkeypatch.setattr(orchestrator, "_embedding_cache_set", _cache_set)
    rows = await orchestrator.embed_text_batch(["ab", "cdef", "ab"])
    assert rows == [[2.0, 0.0], [4.0, 0.0], [2.0, 0.0]]
    assert orchestrator.fastembed_adapter_fallbacks == 2


def test_qdrant_mode_helpers(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "QDRANT_SEARCH_HNSW_EF", 64)
    monkeypatch.setattr(orchestrator, "QDRANT_SEARCH_MODE_HNSW_EF_ENV", "{\"fast\":32,\"deep\":140}")
    monkeypatch.setattr(orchestrator, "QDRANT_SEARCH_MODE_LIMIT_CAPS_ENV", "{\"fast\":40,\"balanced\":90,\"deep\":130}")
    assert orchestrator._qdrant_mode_hnsw_ef("fast") == 32
    assert orchestrator._qdrant_mode_hnsw_ef("balanced") == 64
    assert orchestrator._qdrant_mode_limit_cap("fast", 100) == 40
    assert orchestrator._qdrant_mode_limit_cap("deep", 200) == 130


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
async def test_tool_ops_queue_status_reports_pressure(monkeypatch: pytest.MonkeyPatch):
    queue = asyncio.Queue(maxsize=10)
    for _ in range(8):
        queue.put_nowait({"event_id": str(_)})
    monkeypatch.setattr(orchestrator, "memory_write_queue", queue)
    monkeypatch.setattr(orchestrator, "MEMORY_WRITE_QUEUE_MAX", 10)
    monkeypatch.setattr(
        orchestrator,
        "outbox_health",
        {
            "lastError": "MindsDB autosync error: verification failed",
            "lastProcessedAt": "2026-03-12T00:00:00Z",
            "lastBatchSize": 4,
        },
    )
    monkeypatch.setattr(orchestrator, "LETTA_ADMISSION_BACKLOG_SOFT_LIMIT", 500)
    monkeypatch.setattr(orchestrator, "LETTA_ADMISSION_BACKLOG_HARD_LIMIT", 1500)
    monkeypatch.setattr(orchestrator, "LETTA_ADMISSION_ENABLED", True)
    monkeypatch.setattr(orchestrator, "FANOUT_BACKPRESSURE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "FANOUT_BACKPRESSURE_TARGETS", ["letta"])
    monkeypatch.setattr(orchestrator, "FANOUT_BACKPRESSURE_QUEUE_HIGH_WATERMARK", 0.65)
    monkeypatch.setattr(orchestrator, "FANOUT_BACKPRESSURE_MAX_SLEEP_SECS", 1.25)
    monkeypatch.setattr(orchestrator, "memory_write_queue_processed", 25)
    monkeypatch.setattr(orchestrator, "memory_write_queue_dropped", 2)
    monkeypatch.setattr(orchestrator, "letta_admission_last_reason", "excluded_file_pattern")
    monkeypatch.setattr(orchestrator, "letta_admission_last_backlog", 900)

    async def _summary():
        return {
            "by_status": {"pending": 900, "retrying": 600, "running": 12, "failed": 5, "succeeded": 200},
            "by_target": {"letta": {"pending": 800, "retrying": 100, "running": 10}},
        }

    async def _deadletters(_statuses, limit=100, target=None):
        return [
            {"target": "mindsdb", "id": 1},
            {"target": "mindsdb", "id": 2},
            {"target": "letta", "id": 3},
        ][:limit]

    monkeypatch.setattr(orchestrator, "get_fanout_summary", _summary)
    monkeypatch.setattr(orchestrator, "list_fanout_jobs", _deadletters)
    payload = orchestrator.OpsQueueStatusRequest(
        include_deadletters=True,
        deadletter_limit=10,
        queue_high_watermark=0.7,
    )
    result = await orchestrator.tool_ops_queue_status(payload)
    assert result["queue"]["highWatermarkExceeded"] is True
    assert result["deadletters"]["byTarget"]["mindsdb"] == 2
    assert result["lettaAdmission"]["backlog"] == 900
    assert result["nextActions"]


@pytest.mark.asyncio
async def test_tool_memory_write_batch_request_and_item_idempotency(monkeypatch: pytest.MonkeyPatch):
    calls = {"count": 0}

    async def _write_memory(payload: Any, _request: Request):
        calls["count"] += 1
        return {
            "ok": True,
            "event_id": f"evt-{calls['count']}",
            "warnings": [],
            "fanout": {"mongo_raw": "succeeded"},
        }

    monkeypatch.setattr(orchestrator, "write_memory", _write_memory)
    monkeypatch.setattr(orchestrator, "MEMORY_WRITE_BATCH_MAX_ITEMS", 16)
    monkeypatch.setattr(orchestrator, "MEMORY_WRITE_BATCH_IDEMPOTENCY_TTL_SECS", 900.0)
    monkeypatch.setattr(orchestrator, "memory_write_batch_request_idempotency_seen", orchestrator.OrderedDict())
    monkeypatch.setattr(orchestrator, "memory_write_batch_item_idempotency_seen", orchestrator.OrderedDict())
    monkeypatch.setattr(
        orchestrator,
        "memory_write_batch_metrics",
        {
            "accepted": 0,
            "rejected": 0,
            "requestIdempotentHits": 0,
            "itemIdempotentHits": 0,
            "itemSuccesses": 0,
            "itemFailures": 0,
        },
    )

    payload = orchestrator.MemoryWriteBatchRequest(
        idempotencyKey="req-1",
        items=[
            orchestrator.MemoryWriteBatchItem(
                projectName="alpha",
                fileName="notes/a.md",
                content="hello",
                itemId="a1",
                idempotencyKey="item-1",
            ),
            orchestrator.MemoryWriteBatchItem(
                projectName="alpha",
                fileName="notes/b.md",
                content="hello",
                itemId="a2",
                idempotencyKey="item-1",
            ),
        ],
    )

    first = await orchestrator.tool_memory_write_batch(payload)
    second = await orchestrator.tool_memory_write_batch(payload)
    assert first["ok"] is True
    assert first["succeeded"] == 2
    assert calls["count"] == 1  # second row replayed via item idempotency
    assert second["idempotentReplay"] is True
    assert second["idempotencyScope"] == "request"
    assert orchestrator.memory_write_batch_metrics["requestIdempotentHits"] >= 1


@pytest.mark.asyncio
async def test_tool_feedback_submit_request_idempotency(monkeypatch: pytest.MonkeyPatch):
    calls = {"count": 0}

    async def _create_feedback_record(
        project: str | None,
        user_id: str | None,
        source: str | None,
        task_id: str | None,
        rating: int | None,
        sentiment: str | None,
        tags: list[str] | None,
        content: str | None,
        topic_path: str | None,
        metadata: dict[str, Any] | None,
    ) -> dict[str, Any]:
        calls["count"] += 1
        return {
            "id": f"fb-{calls['count']}",
            "project": project,
            "user_id": user_id,
            "source": source,
            "task_id": task_id,
            "rating": rating,
            "sentiment": sentiment,
            "tags": tags,
            "content": content,
            "topic_path": topic_path,
            "metadata": metadata,
            "created_at": "2026-03-15T00:00:00Z",
        }

    async def _persist(_record: dict[str, Any]) -> tuple[bool, str | None]:
        return True, None

    async def _list_feedback(_project: str | None, _user_id: str | None, _source: str | None, _limit: int):
        return [{"source": "agent", "content": "great", "rating": 5, "topic_path": "runbooks"}]

    monkeypatch.setattr(orchestrator, "create_feedback_record", _create_feedback_record)
    monkeypatch.setattr(orchestrator, "_persist_feedback_to_memory", _persist)
    monkeypatch.setattr(orchestrator, "list_feedback_records", _list_feedback)
    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", True)
    monkeypatch.setattr(orchestrator, "feedback_submit_idempotency_seen", orchestrator.OrderedDict())
    monkeypatch.setattr(
        orchestrator,
        "feedback_submit_metrics",
        {"accepted": 0, "rejected": 0, "idempotentHits": 0, "persisted": 0, "persistFailed": 0},
    )

    payload = orchestrator.FeedbackSubmitRequest(
        project="alpha",
        user_id="u1",
        rating=5,
        content="Great retrieval quality",
        tags=["quality", "qdrant"],
        idempotencyKey="feedback-1",
        include_preferences=True,
    )
    first = await orchestrator.tool_feedback_submit(payload)
    second = await orchestrator.tool_feedback_submit(payload)

    assert first["ok"] is True
    assert first["feedback"]["id"] == "fb-1"
    assert first["learning"]["memoryIndexed"] is True
    assert second["idempotentReplay"] is True
    assert second["idempotencyScope"] == "request"
    assert calls["count"] == 1
    assert orchestrator.feedback_submit_metrics["accepted"] == 1
    assert orchestrator.feedback_submit_metrics["idempotentHits"] == 1


@pytest.mark.asyncio
async def test_tool_feedback_submit_rejects_malformed_tag(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "feedback_submit_idempotency_seen", orchestrator.OrderedDict())
    monkeypatch.setattr(
        orchestrator,
        "feedback_submit_metrics",
        {"accepted": 0, "rejected": 0, "idempotentHits": 0, "persisted": 0, "persistFailed": 0},
    )
    with pytest.raises(orchestrator.HTTPException) as exc:
        await orchestrator.tool_feedback_submit(
            orchestrator.FeedbackSubmitRequest(
                project="alpha",
                content="bad tag input",
                tags=["has space"],
            )
        )
    assert exc.value.status_code == 422
    assert orchestrator.feedback_submit_metrics["rejected"] == 1


def test_sanitize_mindsdb_query_text_truncates_controls():
    raw = "alpha\x00\x1f\t\tbeta " + ("x" * 1000)
    cleaned = orchestrator._sanitize_mindsdb_query_text(raw)
    assert "\x00" not in cleaned
    assert "\x1f" not in cleaned
    assert len(cleaned) <= orchestrator.MINDSDB_QUERY_TEXT_MAX_CHARS


@pytest.mark.asyncio
async def test_get_retrieval_source_quality_summarizes_deltas(monkeypatch: pytest.MonkeyPatch):
    async def _metrics(_limit: int):
        return {
            "updatedAt": "2026-03-12T00:00:00Z",
            "latency": {
                "sources": {
                    "qdrant": {"requests": 100, "timeouts": 2, "p50Ms": 40, "p95Ms": 500, "p99Ms": 900},
                    "letta": {"requests": 100, "timeouts": 80, "p50Ms": 10000, "p95Ms": 39000, "p99Ms": 40000},
                }
            },
            "recallQuality": {
                "bySource": {
                    "qdrant": {"requests": 1000, "errors": 1, "errorRate": 0.001},
                    "letta": {"requests": 1000, "errors": 100, "errorRate": 0.1},
                }
            },
        }

    monkeypatch.setattr(orchestrator, "_build_retrieval_metrics_payload", _metrics)
    result = await orchestrator.get_retrieval_source_quality(limit=5)
    assert result["baselineSource"] == "qdrant"
    assert result["sources"][0]["source"] == "letta"
    assert result["sources"][0]["errorRateDeltaVsQdrant"] > 0
    assert any("Letta timeout rate is high" in row for row in result["recommendations"])


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
    monkeypatch.setattr(orchestrator, "RETRIEVAL_SYNC_ASYNC_SPLIT_ENABLED", False)
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
    assert "letta" in debug["staged_fetch"]["slow_sources_skipped"]


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
        "exit_manager__state__20260302T101500Z.json",
        "root",
        "state snapshot",
        include_short_summary=False,
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


def test_memory_bank_telemetry_classifier_helpers():
    assert orchestrator._looks_memory_bank_telemetry_file(
        "arena__mint_agg-20260310T020603091Z-part1of1.json",
        "root",
    )
    assert orchestrator._looks_memory_bank_telemetry_file(
        "telemetry/queue__state__20260310T020603091Z.json",
        "telemetry",
    )
    assert not orchestrator._looks_memory_bank_telemetry_file(
        "runbooks/profitability/baseline_ladder.md",
        "runbooks/profitability",
    )
    assert not orchestrator._looks_memory_bank_telemetry_file(
        "notes/agent-checkpoints/2026-03-14-mindsdb-telemetry-phase-proposal.md",
        "notes/agent-checkpoints",
    )
    assert orchestrator._looks_memory_bank_telemetry_file(
        "notes/telemetry-overview.md",
        "telemetry/live",
    )


@pytest.mark.asyncio
async def test_search_memory_bank_lexical_ranks_after_full_scan(monkeypatch: pytest.MonkeyPatch):
    async def _files(_project: str):
        return [f"noise/file_{idx}.json" for idx in range(80)] + [
            "runbooks/profitability/baseline_ladder.md",
        ]

    async def _read(_project: str, file_name: str, **_kwargs):
        if file_name.endswith("baseline_ladder.md"):
            return "Baseline ladder tuning notes with concrete profitability changes."
        return ""

    async def _summary(content: str, max_length: int = 500):
        return content[:max_length]

    monkeypatch.setattr(orchestrator, "list_files", _files)
    monkeypatch.setattr(orchestrator, "read_project_file", _read)
    monkeypatch.setattr(orchestrator, "summarize_content", _summary)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_FILES_PER_PROJECT", 8)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_SCAN_LIMIT", 12)
    monkeypatch.setattr(orchestrator, "MEMORY_BANK_TELEMETRY_GUARD_ENABLED", False)

    rows = await orchestrator.search_memory_bank_lexical(
        "profitability baseline ladder",
        project_filter="alpha",
        limit=6,
        time_budget_secs=5.0,
    )
    assert rows
    assert rows[0]["file"] == "runbooks/profitability/baseline_ladder.md"


@pytest.mark.asyncio
async def test_search_memory_bank_lexical_skips_telemetry_unless_query_targets_it(
    monkeypatch: pytest.MonkeyPatch,
):
    reads = {"count": 0}

    async def _files(_project: str):
        return ["telemetry/queue__state__20260310T020603091Z.json"]

    async def _read(_project: str, _file_name: str, **_kwargs):
        reads["count"] += 1
        return "{\"queueDepth\": 42}"

    async def _summary(content: str, max_length: int = 500):
        return content[:max_length]

    monkeypatch.setattr(orchestrator, "list_files", _files)
    monkeypatch.setattr(orchestrator, "read_project_file", _read)
    monkeypatch.setattr(orchestrator, "summarize_content", _summary)
    monkeypatch.setattr(orchestrator, "MEMORY_BANK_TELEMETRY_GUARD_ENABLED", True)

    normal_rows = await orchestrator.search_memory_bank_lexical(
        "profitability baseline ladder",
        project_filter="alpha",
        limit=5,
        time_budget_secs=3.0,
    )
    assert normal_rows == []
    assert reads["count"] == 0

    telemetry_rows = await orchestrator.search_memory_bank_lexical(
        "telemetry queue health",
        project_filter="alpha",
        limit=5,
        time_budget_secs=3.0,
    )
    assert telemetry_rows
    assert reads["count"] == 1


@pytest.mark.asyncio
async def test_search_memory_bank_lexical_backend_disabled_short_circuits(monkeypatch: pytest.MonkeyPatch):
    async def _unexpected(*_args, **_kwargs):
        raise AssertionError("list_files should not be called when memory-bank backend is disabled")

    monkeypatch.setattr(orchestrator, "MEMORY_BANK_SPIKE_BACKEND", "disabled")
    monkeypatch.setattr(orchestrator, "list_files", _unexpected)

    rows = await orchestrator.search_memory_bank_lexical(
        "profitability baseline ladder",
        project_filter="alpha",
        limit=5,
        time_budget_secs=2.0,
    )
    assert rows == []


@pytest.mark.asyncio
async def test_search_memory_bank_lexical_backend_override_can_reenable_native(monkeypatch: pytest.MonkeyPatch):
    async def _files(_project: str):
        return ["runbooks/profitability/baseline_ladder.md"]

    async def _read(_project: str, file_name: str, **_kwargs):
        if file_name.endswith("baseline_ladder.md"):
            return "Baseline ladder tuning notes with concrete profitability changes."
        return ""

    async def _summary(content: str, max_length: int = 500):
        return content[:max_length]

    monkeypatch.setattr(orchestrator, "MEMORY_BANK_SPIKE_BACKEND", "disabled")
    monkeypatch.setattr(orchestrator, "list_files", _files)
    monkeypatch.setattr(orchestrator, "read_project_file", _read)
    monkeypatch.setattr(orchestrator, "summarize_content", _summary)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_FILES_PER_PROJECT", 8)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_SCAN_LIMIT", 12)

    rows = await orchestrator.search_memory_bank_lexical(
        "profitability baseline ladder",
        project_filter="alpha",
        limit=6,
        time_budget_secs=5.0,
        backend_mode_override="native",
    )
    assert rows
    assert rows[0]["file"] == "runbooks/profitability/baseline_ladder.md"


@pytest.mark.asyncio
async def test_search_memory_bank_lexical_spike_fallbacks_to_native(monkeypatch: pytest.MonkeyPatch):
    async def _files(_project: str):
        return ["runbooks/profitability/baseline_ladder.md"]

    async def _read(_project: str, file_name: str, **_kwargs):
        if file_name.endswith("baseline_ladder.md"):
            return "Baseline ladder tuning notes with concrete profitability changes."
        return ""

    async def _summary(content: str, max_length: int = 500):
        return content[:max_length]

    start_fallbacks = int(orchestrator.memory_bank_spike_fallbacks)
    monkeypatch.setattr(orchestrator, "MEMORY_BANK_SPIKE_BACKEND", "meilisearch_spike")
    monkeypatch.setattr(orchestrator, "MEMORY_BANK_SPIKE_HTTP_URL", "")
    monkeypatch.setattr(orchestrator, "MEMORY_BANK_SPIKE_FALLBACK_TO_NATIVE", True)
    monkeypatch.setattr(orchestrator, "list_files", _files)
    monkeypatch.setattr(orchestrator, "read_project_file", _read)
    monkeypatch.setattr(orchestrator, "summarize_content", _summary)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_FILES_PER_PROJECT", 8)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_MEMORY_SCAN_LIMIT", 12)
    monkeypatch.setattr(orchestrator, "MEMORY_BANK_TELEMETRY_GUARD_ENABLED", True)

    rows = await orchestrator.search_memory_bank_lexical(
        "profitability baseline ladder",
        project_filter="alpha",
        limit=6,
        time_budget_secs=5.0,
    )
    assert rows
    assert rows[0]["file"] == "runbooks/profitability/baseline_ladder.md"
    assert int(orchestrator.memory_bank_spike_fallbacks) >= start_fallbacks + 1


@pytest.mark.asyncio
async def test_memory_bank_cleanup_chunked_batches_projects(monkeypatch: pytest.MonkeyPatch):
    async def _projects():
        return ["alpha", "beta", "gamma"]

    async def _cleanup(*, project: str | None, limit: int, dry_run: bool):
        return {
            "ok": True,
            "dryRun": dry_run,
            "project": project,
            "scanned": 5,
            "matched": 2,
            "alreadyProcessed": 1,
            "selected": 1,
            "updated": 0,
            "failed": 0,
            "limit": limit,
        }

    monkeypatch.setattr(orchestrator, "list_projects", _projects)
    monkeypatch.setattr(orchestrator, "run_memory_bank_telemetry_cleanup", _cleanup)

    result = await orchestrator.run_memory_bank_telemetry_cleanup_chunked(
        start_after=None,
        project_batch=2,
        per_project_limit=50,
        dry_run=True,
    )

    assert result["ok"] is True
    assert result["nextStartAfter"] == "beta"
    assert result["totals"]["projectsProcessed"] == 2
    assert result["totals"]["selected"] == 2
    assert len(result["projects"]) == 2


def test_merge_rows_respects_raw_intent_low_value_policy(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "RETRIEVAL_LOW_VALUE_NON_LETTA_SUPPRESS", True)
    monkeypatch.setattr(orchestrator, "RETRIEVAL_INTENT_RAW_DISABLE_LOW_VALUE_SUPPRESS", True)
    sample_rows = {
        "qdrant": [
            {
                "project": "alpha",
                "file": "exit_manager__state__20260302T101500Z.json",
                "summary": "state snapshot",
                "score": 0.9,
                "source": "qdrant",
                "topic_path": "root",
            }
        ]
    }
    weights = {"qdrant": 1.0}

    decision_rows = orchestrator._merge_federated_rows(
        sample_rows,
        weights,
        set(),
        set(),
        learning_enabled=False,
        query="execution blockers",
        retrieval_intent="decision",
    )
    raw_rows = orchestrator._merge_federated_rows(
        sample_rows,
        weights,
        set(),
        set(),
        learning_enabled=False,
        query="execution blockers",
        retrieval_intent="raw",
    )

    assert decision_rows[0]["low_value_suppressed"] is True
    assert raw_rows[0]["low_value_suppressed"] is False


@pytest.mark.asyncio
async def test_mindsdb_lz4_circuit_breaker_skips_repeated_queries(monkeypatch: pytest.MonkeyPatch):
    calls = {"execute": 0}

    async def _ensure():
        return None

    async def _execute(_sql: str):
        calls["execute"] += 1
        raise RuntimeError("[file/files]: LZ4 decompress failed: ERROR_decompressionFailed")

    monkeypatch.setattr(orchestrator, "MINDSDB_ENABLED", True)
    monkeypatch.setattr(orchestrator, "MINDSDB_AUTOSYNC", True)
    monkeypatch.setattr(orchestrator, "MINDSDB_RETRIEVAL_CIRCUIT_ENABLED", True)
    monkeypatch.setattr(orchestrator, "MINDSDB_RETRIEVAL_LZ4_COOLDOWN_SECS", 120.0)
    monkeypatch.setattr(orchestrator, "ensure_mindsdb_table", _ensure)
    monkeypatch.setattr(orchestrator, "_mindsdb_execute", _execute)
    monkeypatch.setattr(orchestrator, "mindsdb_retrieval_lz4_cooldown_until_monotonic", 0.0)
    monkeypatch.setattr(orchestrator, "mindsdb_retrieval_lz4_hits", 0)
    monkeypatch.setattr(orchestrator, "mindsdb_retrieval_lz4_skipped", 0)
    monkeypatch.setattr(orchestrator, "mindsdb_retrieval_log_last_at", {})

    first = await orchestrator.search_mindsdb_memory("queue pressure", limit=5)
    second = await orchestrator.search_mindsdb_memory("queue pressure", limit=5)

    assert first == []
    assert second == []
    assert calls["execute"] == 1
    assert orchestrator.mindsdb_retrieval_lz4_hits == 1
    assert orchestrator.mindsdb_retrieval_lz4_skipped >= 1


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
async def test_run_letta_auto_prune_once_skips_below_threshold(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_PRUNE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_PRUNE_BACKLOG_TRIGGER", 10)
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_PRUNE_LIMIT", 50)
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_PRUNE_TIMEOUT_SECS", 5.0)
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_PRUNE_STATUSES", ["pending", "retrying"])
    orchestrator.letta_auto_prune_state.update(
        {
            "lastRunAt": None,
            "lastDurationMs": None,
            "lastError": None,
            "lastDeleted": 0,
            "lastBacklogBefore": 0,
            "lastBacklogAfter": 0,
            "lastSkippedReason": None,
            "runs": 0,
            "lastResult": {},
        }
    )

    async def _summary():
        return {"by_target": {"letta": {"pending": 4, "retrying": 1}}}

    async def _prune(*, statuses: list[str], limit: int, dry_run: bool):
        raise AssertionError("prune should not run below threshold")

    monkeypatch.setattr(orchestrator, "get_fanout_summary", _summary)
    monkeypatch.setattr(orchestrator, "prune_letta_low_value_outbox", _prune)

    result = await orchestrator.run_letta_auto_prune_once()
    assert result["ran"] is False
    assert result["skipped"] == "below_threshold"
    assert result["backlog"] == 5
    assert orchestrator.letta_auto_prune_state["runs"] == 1
    assert orchestrator.letta_auto_prune_state["lastSkippedReason"] == "below_threshold"
    assert orchestrator.letta_auto_prune_state["lastError"] is None


@pytest.mark.asyncio
async def test_run_letta_auto_prune_once_prunes_when_threshold_met(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_PRUNE_ENABLED", True)
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_PRUNE_BACKLOG_TRIGGER", 10)
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_PRUNE_LIMIT", 123)
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_PRUNE_TIMEOUT_SECS", 5.0)
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_PRUNE_STATUSES", ["pending", "retrying"])
    orchestrator.letta_auto_prune_state.update(
        {
            "lastRunAt": None,
            "lastDurationMs": None,
            "lastError": None,
            "lastDeleted": 0,
            "lastBacklogBefore": 0,
            "lastBacklogAfter": 0,
            "lastSkippedReason": None,
            "runs": 0,
            "lastResult": {},
        }
    )
    seen: dict[str, Any] = {}

    async def _summary():
        return {"by_target": {"letta": {"pending": 20, "retrying": 5}}}

    async def _fresh_summary():
        return {"by_target": {"letta": {"pending": 11, "retrying": 2, "running": 1}}}

    async def _prune(*, statuses: list[str], limit: int, dry_run: bool):
        seen["statuses"] = statuses
        seen["limit"] = limit
        seen["dry_run"] = dry_run
        return {
            "backend": "sqlite",
            "statuses": statuses,
            "beforePending": 25,
            "afterPending": 13,
            "scanned": 40,
            "matched": 12,
            "deleted": 12,
            "dryRun": False,
            "limit": limit,
            "matchedExcluded": 8,
            "matchedLowValue": 9,
        }

    monkeypatch.setattr(orchestrator, "get_fanout_summary", _summary)
    monkeypatch.setattr(orchestrator, "_query_fanout_summary_uncached", _fresh_summary)
    monkeypatch.setattr(orchestrator, "prune_letta_low_value_outbox", _prune)

    result = await orchestrator.run_letta_auto_prune_once()
    assert result["ran"] is True
    assert result["backlogBefore"] == 25
    assert result["backlogAfter"] == 14
    assert result["prune"]["deleted"] == 12
    assert seen == {"statuses": ["pending", "retrying"], "limit": 123, "dry_run": False}
    assert orchestrator.letta_auto_prune_state["runs"] == 1
    assert orchestrator.letta_auto_prune_state["lastDeleted"] == 12
    assert orchestrator.letta_auto_prune_state["lastBacklogBefore"] == 25
    assert orchestrator.letta_auto_prune_state["lastBacklogAfter"] == 14
    assert orchestrator.letta_auto_prune_state["lastSkippedReason"] is None


@pytest.mark.asyncio
async def test_run_sink_retention_once_collects_partial_errors(monkeypatch: pytest.MonkeyPatch):
    async def _qdrant():
        return {"enabled": True, "deleted": 2}

    async def _mongo():
        raise RuntimeError("mongo unavailable")

    async def _mindsdb():
        return {"enabled": True, "deleted": 1}

    async def _letta():
        return {"enabled": True, "deleted": 0}

    monkeypatch.setattr(orchestrator, "_run_qdrant_low_value_retention_once", _qdrant)
    monkeypatch.setattr(orchestrator, "_run_mongo_low_value_retention_once", _mongo)
    monkeypatch.setattr(orchestrator, "_run_mindsdb_low_value_retention_once", _mindsdb)
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
    assert result["sinks"]["mindsdb"]["deleted"] == 1
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

    monkeypatch.setattr(orchestrator, "CONTEXTLATTICE_READ_FAIL_OPEN_ENABLED", True)
    monkeypatch.setattr(orchestrator, "CONTEXTLATTICE_READ_CACHE_MAX_KEYS", 64)
    monkeypatch.setattr(orchestrator, "CONTEXTLATTICE_READ_CACHE_FRESH_TTL_SECS", 0.0)
    monkeypatch.setattr(orchestrator, "CONTEXTLATTICE_READ_CACHE_STALE_MAX_SECS", 3600.0)
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

    monkeypatch.setattr(orchestrator, "CONTEXTLATTICE_READ_FAIL_OPEN_ENABLED", True)
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

    monkeypatch.setattr(orchestrator, "CONTEXTLATTICE_READ_FAIL_OPEN_ENABLED", True)
    monkeypatch.setattr(orchestrator, "CONTEXTLATTICE_READ_CACHE_MAX_KEYS", 64)
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
    monkeypatch.setattr(orchestrator, "MEMORY_BANK_TELEMETRY_GUARD_ENABLED", False)
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
            "qdrant_collection": "contextlattice_notes",
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
    monkeypatch.setattr(orchestrator, "MEMORY_BANK_TELEMETRY_GUARD_ENABLED", False)
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


@pytest.mark.asyncio
async def test_write_memory_skips_memory_bank_for_telemetry_low_value(monkeypatch: pytest.MonkeyPatch):
    finalized: list[dict[str, object]] = []

    async def _fake_finalize(
        item: dict[str, object],
        *,
        worker_id: int | None,
        persisted_to_memory_bank: bool,
    ) -> None:
        finalized.append(
            {
                "project": item.get("project"),
                "file": item.get("file"),
                "worker_id": worker_id,
                "persisted_to_memory_bank": persisted_to_memory_bank,
            }
        )

    async def _fake_summarize(content: str, max_length: int = 500):
        return content[:max_length]

    async def _persist_raw(_event: dict[str, object]):
        return True, None

    async def _fanout_summary():
        return {"by_status": {}, "by_target": {}}

    async def _should_skip_duplicate(_dedupe_key: str):
        return False

    async def _should_skip_latest(_project: str, _file: str, _hash: str):
        return False

    async def _unexpected_enqueue(_item: dict[str, object]):
        raise AssertionError("memory_bank enqueue should be skipped for telemetry low-value writes")

    monkeypatch.setattr(orchestrator, "_finalize_memory_write_item", _fake_finalize)
    monkeypatch.setattr(orchestrator, "summarize_content", _fake_summarize)
    monkeypatch.setattr(orchestrator, "persist_raw_event_to_mongo", _persist_raw)
    monkeypatch.setattr(orchestrator, "get_fanout_summary", _fanout_summary)
    monkeypatch.setattr(orchestrator, "should_skip_duplicate_memory_write", _should_skip_duplicate)
    monkeypatch.setattr(orchestrator, "should_skip_unchanged_latest_hash", _should_skip_latest)
    monkeypatch.setattr(orchestrator, "_enqueue_memory_bank_write", _unexpected_enqueue)
    monkeypatch.setattr(orchestrator, "MEMORY_BANK_TELEMETRY_GUARD_ENABLED", True)
    monkeypatch.setattr(orchestrator, "LETTA_AUTO_SESSION_ID", "")

    request = SimpleNamespace(state=SimpleNamespace(request_id="test-low-value"))
    payload = orchestrator.MemoryWrite(
        projectName="alpha",
        fileName="telemetry/queue__state__20260310T020603091Z.json",
        content="{\"queueDepth\":42}",
    )
    response = await orchestrator.write_memory(payload, request)
    assert response["ok"] is True
    assert response["fanout"]["memory_bank"] == "skipped_low_value"
    assert response["fanout"]["qdrant"] == "skipped_low_value"
    assert response["fanout"]["mindsdb"] == "skipped_low_value"
    assert response["fanout"]["letta"] == "skipped_low_value"
    assert response["memory_bank_skipped_low_value"] is True
    assert finalized and finalized[0]["persisted_to_memory_bank"] is False


@pytest.mark.asyncio
async def test_enqueue_memory_write_fanout_filters_telemetry_targets(monkeypatch: pytest.MonkeyPatch):
    captured_targets: list[str] = []

    async def _fake_enqueue(event_payload: dict[str, Any], targets: list[str], force_requeue: bool = False):
        del event_payload, force_requeue
        captured_targets.extend(targets)
        return {"inserted": len(targets)}

    monkeypatch.setattr(orchestrator, "enqueue_fanout_outbox", _fake_enqueue)
    monkeypatch.setattr(orchestrator, "memory_write_queue", asyncio.Queue(maxsize=8))
    monkeypatch.setattr(orchestrator, "_letta_target_enabled", lambda: True)
    monkeypatch.setattr(orchestrator, "QDRANT_TELEMETRY_GUARD_ENABLED", True)
    monkeypatch.setattr(orchestrator, "MINDSDB_TELEMETRY_GUARD_ENABLED", True)
    monkeypatch.setattr(orchestrator, "LETTA_TELEMETRY_GUARD_ENABLED", True)
    monkeypatch.setattr(orchestrator, "LANGFUSE_API_KEY", "")

    await orchestrator._enqueue_memory_write_fanout(
        {
            "event_id": "evt-telemetry",
            "project": "alpha",
            "file": "telemetry/queue__state__20260310T020603091Z.json",
            "summary": "queue depth telemetry snapshot",
            "topic_path": "telemetry/queue",
            "letta_session": "agent-1",
            "letta_admit": True,
            "mongo_persisted": False,
        }
    )

    assert captured_targets == ["mongo_raw"]


@pytest.mark.asyncio
async def test_enqueue_memory_write_fanout_keeps_knowledge_targets(monkeypatch: pytest.MonkeyPatch):
    captured_targets: list[str] = []

    async def _fake_enqueue(event_payload: dict[str, Any], targets: list[str], force_requeue: bool = False):
        del event_payload, force_requeue
        captured_targets.extend(targets)
        return {"inserted": len(targets)}

    monkeypatch.setattr(orchestrator, "enqueue_fanout_outbox", _fake_enqueue)
    monkeypatch.setattr(orchestrator, "memory_write_queue", asyncio.Queue(maxsize=8))
    monkeypatch.setattr(orchestrator, "_letta_target_enabled", lambda: True)
    monkeypatch.setattr(orchestrator, "QDRANT_TELEMETRY_GUARD_ENABLED", True)
    monkeypatch.setattr(orchestrator, "MINDSDB_TELEMETRY_GUARD_ENABLED", True)
    monkeypatch.setattr(orchestrator, "LETTA_TELEMETRY_GUARD_ENABLED", True)
    monkeypatch.setattr(orchestrator, "LANGFUSE_API_KEY", "")

    await orchestrator._enqueue_memory_write_fanout(
        {
            "event_id": "evt-knowledge",
            "project": "alpha",
            "file": "runbooks/profitability/baseline.md",
            "summary": "profitability ladder and promotion gates",
            "topic_path": "runbooks/profitability",
            "letta_session": "agent-1",
            "letta_admit": True,
            "mongo_persisted": False,
        }
    )

    assert captured_targets == ["mongo_raw", "qdrant", "postgres_pgvector", "mindsdb", "letta"]


@pytest.mark.asyncio
async def test_purge_telemetry_from_retrieval_sinks_dispatches(monkeypatch: pytest.MonkeyPatch):
    async def _fake_qdrant(*, scan_limit: int, max_deletes: int, dry_run: bool):
        return {"enabled": True, "dryRun": dry_run, "scanned": scan_limit, "deleteCandidates": max_deletes, "deleted": 0}

    async def _fake_mindsdb(*, scan_limit: int, max_deletes: int, dry_run: bool):
        return {"enabled": True, "dryRun": dry_run, "scanned": scan_limit, "deleteCandidates": max_deletes, "deleted": 0}

    async def _fake_letta(*, scan_limit: int, max_deletes: int, dry_run: bool):
        return {"enabled": True, "dryRun": dry_run, "scanned": scan_limit, "deleteCandidates": max_deletes, "deleted": 0}

    monkeypatch.setattr(orchestrator, "_run_qdrant_telemetry_purge_once", _fake_qdrant)
    monkeypatch.setattr(orchestrator, "_run_mindsdb_telemetry_purge_once", _fake_mindsdb)
    monkeypatch.setattr(orchestrator, "_run_letta_telemetry_purge_once", _fake_letta)

    result = await orchestrator.purge_telemetry_from_retrieval_sinks(
        dry_run=True,
        scan_limit=250,
        max_deletes_per_sink=120,
        include_qdrant=True,
        include_mindsdb=True,
        include_letta=True,
    )

    assert result["ok"] is True
    assert result["dryRun"] is True
    assert result["errors"] == {}
    assert set(result["sinks"].keys()) == {"qdrant", "mindsdb", "letta"}
    assert all((result["sinks"][name] or {}).get("deleteCandidates") == 120 for name in result["sinks"])


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
    assert rows[0]["topic_rollup"]["raw_refs"] == ["decisions/a.md"]


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
async def test_retriever_runtime_request_includes_rust_backend_policy(monkeypatch: pytest.MonkeyPatch):
    captured: dict[str, Any] = {}

    class _FakeRetriever:
        async def search_with_grounding(self, request):
            captured["backend_policy"] = dict(getattr(request, "backend_policy", {}) or {})
            return SimpleNamespace(
                results=[{"summary": "ok", "score": 0.9}],
                retrieval_debug={"retrieval_mode": "balanced"},
                warnings=[],
                grounding={"facts": []},
            )

    class _FakeRuntime:
        retriever = _FakeRetriever()

    async def _runtime():
        return _FakeRuntime()

    monkeypatch.setattr(orchestrator, "_get_migration_runtime", _runtime)
    monkeypatch.setattr(orchestrator, "RUST_RETRIEVAL_VECTOR_BACKEND", "qdrant_remote")
    monkeypatch.setattr(orchestrator, "RUST_RETRIEVAL_LEXICAL_BACKEND", "auto")
    monkeypatch.setattr(orchestrator, "RUST_RETRIEVAL_BACKEND_STRICT", False)

    _results, debug, _warnings, _grounding = await orchestrator._retriever_search_with_grounding_via_runtime(
        query="alpha",
        limit=5,
        project_filter="alpha",
        topic_filter=None,
        sources=["qdrant", "topic_rollups"],
        source_weights={"qdrant": 1.0},
        preferences={
            "rust_backend_policy": {
                "vector_backend": "usearch_ann",
                "lexical_backend": "tantivy_lexical",
                "memory_bank_backend": "quickwit_spike",
                "strict": True,
            }
        },
        rerank_with_learning=False,
        retrieval_mode="balanced",
        retrieval_intent="decision",
        agent_profile=None,
        auto_escalate=False,
        query_expansion=False,
    )
    assert captured["backend_policy"]["vector_backend"] == "usearch_ann"
    assert captured["backend_policy"]["lexical_backend"] == "tantivy_lexical"
    assert captured["backend_policy"]["memory_bank_backend"] == "quickwit_spike"
    assert captured["backend_policy"]["strict"] is True
    assert debug["runtime"]["rust_backend_policy"]["vector_backend"] == "usearch_ann"


def test_normalize_memory_bank_backend_accepts_extended_spike_modes():
    for backend in ("lancedb_spike", "trieve_spike", "helixdb_spike"):
        assert (
            orchestrator._normalize_memory_bank_backend_choice(backend, default="native")
            == backend
        )


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
        {
            "request": {
                "query": "alpha",
                "limit": 4,
                "project_filter": "proj-a",
                "backend_policy": {
                    "vector_backend": "usearch_ann",
                    "lexical_backend": "tantivy_lexical",
                    "memory_bank_backend": "quickwit_spike",
                    "strict": True,
                },
            }
        }
    )
    assert response["results"]
    assert captured["query"] == "alpha"
    assert captured["project_filter"] == "proj-a"
    assert captured["limit"] == 4
    assert captured["backend_policy"]["vector_backend"] == "usearch_ann"
    assert captured["backend_policy"]["memory_bank_backend"] == "quickwit_spike"


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
