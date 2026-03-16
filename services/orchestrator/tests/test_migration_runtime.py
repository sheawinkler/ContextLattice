from __future__ import annotations

import asyncio
from pathlib import Path
import sys

import pytest


RUNTIME_ROOT = Path(__file__).resolve().parents[1]
if str(RUNTIME_ROOT) not in sys.path:
    sys.path.insert(0, str(RUNTIME_ROOT))

from runtime.flags import MigrationFlags
from runtime.interfaces import MemoryWriteRequest, RetrievalRequest
from runtime.python_impl import JsonMergeStateDelta
from runtime.registry import RuntimeCallbacks, build_runtime


@pytest.mark.parametrize(
    "old_state,new_state,expected_set,expected_unset",
    [
        ({"a": 1}, {"a": 1, "b": 2}, {"b": 2}, []),
        ({"a": 1, "b": 2}, {"a": 3}, {"a": 3}, ["b"]),
    ],
)
def test_state_delta_diff_and_apply(old_state, new_state, expected_set, expected_unset):
    delta = JsonMergeStateDelta()
    patch = delta.diff(old_state, new_state)
    assert patch["set"] == expected_set
    assert patch["unset"] == expected_unset
    assert delta.apply(old_state, patch) == new_state


@pytest.mark.asyncio
async def test_runtime_build_defaults_to_python_impls():
    async def _write(payload: MemoryWriteRequest):
        return {"event_id": f"{payload.project}::{payload.file_name}"}

    async def _read(project: str, file_name: str):
        return f"{project}/{file_name}"

    async def _list_files(project: str):
        return ["a.md", "b.md"]

    async def _neighbors(query: str, limit: int, topic: str | None):
        return [{"summary": query, "limit": limit, "topic": topic}]

    async def _federated(*_args, **_kwargs):
        return ([{"summary": "ok", "score": 1.0}], {"retrieval_mode": "balanced"}, [])

    async def _pipeline(*_args, **_kwargs):
        return ([{"summary": "ok", "score": 1.0}], {"retrieval_mode": "balanced"}, [], {"facts": []})

    async def _submit(*_args, **_kwargs):
        return {"id": "task-1", "status": "queued"}

    async def _claim(_worker: str | None):
        return {"id": "task-1", "status": "running"}

    async def _update(*_args, **_kwargs):
        return {"id": "task-1", "status": "succeeded"}

    async def _retry(*_args, **_kwargs):
        return {"id": "task-1", "status": "queued"}

    async def _metrics():
        return {"ok": True, "totals": {"tasks": 1}}

    callbacks = RuntimeCallbacks(
        write_memory_fn=_write,
        read_memory_fn=_read,
        list_files_fn=_list_files,
        search_neighbors_fn=_neighbors,
        federated_search_fn=_federated,
        recall_pipeline_fn=_pipeline,
        submit_task_fn=_submit,
        claim_task_fn=_claim,
        update_task_fn=_update,
        retry_task_fn=_retry,
        scheduler_metrics_fn=_metrics,
    )
    runtime = build_runtime(MigrationFlags(), callbacks)

    assert runtime.implementation_map["codec"] == "JsonCodec"
    assert runtime.implementation_map["memory_store"] == "PythonMemoryStore"
    assert runtime.implementation_map["retriever"] == "PythonRetriever"
    assert runtime.implementation_map["scheduler"] == "PythonScheduler"

    response = await runtime.retriever.search_with_grounding(RetrievalRequest(query="hello"))
    assert response.results
    assert response.retrieval_debug["retrieval_mode"] == "balanced"


@pytest.mark.asyncio
async def test_runtime_build_uses_proxy_impls_when_flags_enabled(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("CONTEXTLATTICE_ENGINE_URL", "")
    monkeypatch.setenv("CONTEXTLATTICE_GO_ORCHESTRATOR_URL", "")

    async def _noop(*_args, **_kwargs):
        return {}

    async def _pipeline(*_args, **_kwargs):
        return ([], {}, [], {})

    async def _claim(_worker: str | None):
        return None

    async def _metrics():
        return {"ok": True}

    callbacks = RuntimeCallbacks(
        write_memory_fn=_noop,
        read_memory_fn=lambda *_args, **_kwargs: asyncio.sleep(0, result=""),
        list_files_fn=lambda *_args, **_kwargs: asyncio.sleep(0, result=[]),
        search_neighbors_fn=lambda *_args, **_kwargs: asyncio.sleep(0, result=[]),
        federated_search_fn=lambda *_args, **_kwargs: asyncio.sleep(0, result=([], {}, [])),
        recall_pipeline_fn=_pipeline,
        submit_task_fn=_noop,
        claim_task_fn=_claim,
        update_task_fn=lambda *_args, **_kwargs: asyncio.sleep(0, result=None),
        retry_task_fn=lambda *_args, **_kwargs: asyncio.sleep(0, result=None),
        scheduler_metrics_fn=_metrics,
    )
    runtime = build_runtime(
        MigrationFlags(
            use_rust_codec=True,
            use_rust_memory=True,
            use_rust_retrieval=True,
            use_go_orchestrator=True,
        ),
        callbacks,
    )
    assert runtime.implementation_map["codec"] == "RustCodecBridge"
    assert runtime.implementation_map["memory_store"] == "RustMemoryStoreProxy"
    assert runtime.implementation_map["retriever"] == "RustRetrieverProxy"
    assert runtime.implementation_map["scheduler"] == "GoSchedulerProxy"
