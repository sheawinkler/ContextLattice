from __future__ import annotations

import importlib.util
import json
import sys
import time
from types import SimpleNamespace
from datetime import datetime
from pathlib import Path
from typing import Any

import pytest


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


def test_validate_security_posture_requires_api_key_in_production(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setattr(orchestrator, "MEMMCP_ENV", "production")
    monkeypatch.setattr(orchestrator, "ORCH_SECURITY_STRICT", True)
    monkeypatch.setattr(orchestrator, "ORCH_API_KEY", "")
    monkeypatch.setattr(orchestrator, "ORCH_PUBLIC_STATUS", False)
    monkeypatch.setattr(orchestrator, "ORCH_PUBLIC_DOCS", False)
    with pytest.raises(RuntimeError):
        orchestrator.validate_orchestrator_security_posture()


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

    async def _federated(*args, **kwargs):
        return ([], {"source_errors": {}, "source_counts": {}, "resolved_sources": []}, [])

    monkeypatch.setattr(orchestrator, "LEARNING_LOOP_ENABLED", True)
    monkeypatch.setattr(orchestrator, "list_feedback_records", _raise_feedback)
    monkeypatch.setattr(orchestrator, "federated_search_memory", _federated)

    response = await orchestrator.search_memory(orchestrator.MemorySearch(query="alpha"))
    assert response["results"] == []
    assert any("Preference context unavailable" in warning for warning in response["warnings"])


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

    async def _letta(*args, **kwargs):
        slow_calls["letta"] += 1
        return []

    async def _memory_bank(*args, **kwargs):
        slow_calls["memory_bank"] += 1
        return []

    monkeypatch.setattr(orchestrator, "search_qdrant", _qdrant)
    monkeypatch.setattr(orchestrator, "search_mongo_raw", _mongo)
    monkeypatch.setattr(orchestrator, "search_mindsdb_memory", _mindsdb)
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

    results, debug, _ = await orchestrator.federated_search_memory(
        "alpha",
        limit=5,
        sources=["qdrant", "mongo_raw", "mindsdb", "letta", "memory_bank"],
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
