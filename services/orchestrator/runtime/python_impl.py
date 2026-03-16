from __future__ import annotations

import hashlib
import json
from dataclasses import asdict
from typing import Any, Awaitable, Callable

from .interfaces import (
    Codec,
    MemoryStore,
    MemoryWriteRequest,
    RetrievalRequest,
    RetrievalResponse,
    Retriever,
    Scheduler,
    StateDelta,
    TaskStatusRequest,
    TaskSubmitRequest,
)

try:
    import orjson  # type: ignore
except Exception:  # pragma: no cover - optional dependency
    orjson = None  # type: ignore


class JsonCodec(Codec):
    """Python baseline codec used until Rust bindings are promoted."""

    def encode_state(self, obj: Any) -> bytes:
        if orjson is not None:
            return orjson.dumps(obj)
        return json.dumps(obj, ensure_ascii=True, sort_keys=True).encode("utf-8")

    def decode_state(self, payload: bytes) -> Any:
        if orjson is not None:
            return orjson.loads(payload)
        return json.loads(payload.decode("utf-8"))

    def encode_batch(self, items: list[Any]) -> bytes:
        return self.encode_state(items)

    def decode_batch(self, payload: bytes) -> list[Any]:
        decoded = self.decode_state(payload)
        if isinstance(decoded, list):
            return decoded
        return [decoded]

    def checksum(self, payload: bytes) -> str:
        return hashlib.sha256(payload).hexdigest()


class JsonMergeStateDelta(StateDelta):
    """Small JSON-merge style state delta implementation for phase 1/7."""

    def diff(self, old_state: dict[str, Any], new_state: dict[str, Any]) -> dict[str, Any]:
        delta: dict[str, Any] = {"set": {}, "unset": []}
        old_keys = set(old_state.keys())
        new_keys = set(new_state.keys())
        for key in sorted(new_keys):
            if key not in old_state or old_state.get(key) != new_state.get(key):
                delta["set"][key] = new_state.get(key)
        for key in sorted(old_keys - new_keys):
            delta["unset"].append(key)
        return delta

    def apply(self, state: dict[str, Any], delta: dict[str, Any]) -> dict[str, Any]:
        if not self.validate(delta):
            raise ValueError("invalid state delta")
        updated = dict(state)
        for key, value in (delta.get("set") or {}).items():
            updated[str(key)] = value
        for key in (delta.get("unset") or []):
            updated.pop(str(key), None)
        return updated

    def compose(self, delta_a: dict[str, Any], delta_b: dict[str, Any]) -> dict[str, Any]:
        if not self.validate(delta_a) or not self.validate(delta_b):
            raise ValueError("invalid state delta")
        composed_set = dict(delta_a.get("set") or {})
        composed_unset = set(str(item) for item in (delta_a.get("unset") or []))
        for key, value in (delta_b.get("set") or {}).items():
            key_name = str(key)
            composed_set[key_name] = value
            if key_name in composed_unset:
                composed_unset.remove(key_name)
        for key in (delta_b.get("unset") or []):
            key_name = str(key)
            composed_set.pop(key_name, None)
            composed_unset.add(key_name)
        return {"set": composed_set, "unset": sorted(composed_unset)}

    def validate(self, delta: dict[str, Any]) -> bool:
        if not isinstance(delta, dict):
            return False
        set_block = delta.get("set", {})
        unset_block = delta.get("unset", [])
        return isinstance(set_block, dict) and isinstance(unset_block, list)


def _split_memory_id(memory_id: str) -> tuple[str, str]:
    token = str(memory_id or "").strip()
    if "::" in token:
        project, file_name = token.split("::", 1)
        return project.strip(), file_name.strip()
    if "/" in token:
        project, file_name = token.split("/", 1)
        return project.strip(), file_name.strip()
    raise ValueError(f"invalid memory id: {memory_id}")


class PythonMemoryStore(MemoryStore):
    def __init__(
        self,
        *,
        write_memory_fn: Callable[[MemoryWriteRequest], Awaitable[dict[str, Any]]],
        read_memory_fn: Callable[[str, str], Awaitable[str]],
        list_files_fn: Callable[[str], Awaitable[list[str]]],
        search_neighbors_fn: Callable[[str, int, str | None], Awaitable[list[dict[str, Any]]]] | None = None,
    ):
        self._write_memory_fn = write_memory_fn
        self._read_memory_fn = read_memory_fn
        self._list_files_fn = list_files_fn
        self._search_neighbors_fn = search_neighbors_fn

    async def add_memory(self, payload: MemoryWriteRequest) -> str:
        write_result = await self._write_memory_fn(payload)
        event_id = str((write_result or {}).get("event_id") or "").strip()
        if event_id:
            return event_id
        return f"{payload.project}::{payload.file_name}"

    async def update_memory(self, memory_id: str, patch: dict[str, Any]) -> bool:
        project, file_name = _split_memory_id(memory_id)
        prior = await self._read_memory_fn(project, file_name)
        try:
            previous_payload = json.loads(prior) if prior else {}
        except Exception:
            previous_payload = {"content": prior}
        previous_payload.update(patch or {})
        write_payload = MemoryWriteRequest(
            project=project,
            file_name=file_name,
            content=json.dumps(previous_payload, ensure_ascii=True, sort_keys=True),
        )
        await self._write_memory_fn(write_payload)
        return True

    async def get_memory(self, memory_id: str) -> dict[str, Any] | None:
        project, file_name = _split_memory_id(memory_id)
        body = await self._read_memory_fn(project, file_name)
        if not body:
            return None
        return {
            "id": memory_id,
            "project": project,
            "file": file_name,
            "content": body,
        }

    async def query_neighbors(
        self,
        memory_id: str,
        filters: dict[str, Any] | None = None,
        limit: int = 10,
    ) -> list[dict[str, Any]]:
        project, file_name = _split_memory_id(memory_id)
        if self._search_neighbors_fn is not None:
            query = f"{project} {file_name}"
            topic_filter = None
            if isinstance(filters, dict):
                topic_filter = str(filters.get("topic_path") or "").strip() or None
            return await self._search_neighbors_fn(query, max(1, int(limit)), topic_filter)
        files = await self._list_files_fn(project)
        neighbors: list[dict[str, Any]] = []
        for candidate in files:
            if candidate == file_name:
                continue
            neighbors.append({"project": project, "file": candidate, "score": 0.0})
            if len(neighbors) >= max(1, int(limit)):
                break
        return neighbors

    async def batch_insert(self, items: list[MemoryWriteRequest]) -> list[str]:
        ids: list[str] = []
        for payload in items:
            ids.append(await self.add_memory(payload))
        return ids


class PythonRetriever(Retriever):
    def __init__(
        self,
        *,
        federated_search_fn: Callable[..., Awaitable[tuple[list[dict[str, Any]], dict[str, Any], list[str]]]],
        recall_pipeline_fn: Callable[..., Awaitable[tuple[list[dict[str, Any]], dict[str, Any], list[str], dict[str, Any]]]],
    ):
        self._federated_search_fn = federated_search_fn
        self._recall_pipeline_fn = recall_pipeline_fn

    async def search(self, request: RetrievalRequest) -> list[dict[str, Any]]:
        results, _debug, _warnings = await self._federated_search_fn(
            request.query,
            limit=request.limit,
            project_filter=request.project_filter,
            topic_filter=request.topic_filter,
            sources=request.sources,
            source_weights=request.source_weights,
            preferences=request.preferences,
            rerank_with_learning=request.rerank_with_learning,
            retrieval_mode=request.retrieval_mode,
            retrieval_intent=request.retrieval_intent,
            traffic_class=request.traffic_class,
        )
        return results

    async def batch_search(
        self,
        requests: list[RetrievalRequest],
    ) -> list[list[dict[str, Any]]]:
        batches: list[list[dict[str, Any]]] = []
        for request in requests:
            batches.append(await self.search(request))
        return batches

    async def search_with_grounding(self, request: RetrievalRequest) -> RetrievalResponse:
        results, retrieval_debug, warnings, grounding = await self._recall_pipeline_fn(
            query=request.query,
            limit=request.limit,
            project_filter=request.project_filter,
            topic_filter=request.topic_filter,
            sources=request.sources,
            source_weights=request.source_weights,
            preferences=request.preferences,
            rerank_with_learning=request.rerank_with_learning,
            retrieval_mode=request.retrieval_mode,
            retrieval_intent=request.retrieval_intent,
            agent_profile=request.agent_profile,
            auto_escalate=request.auto_escalate,
            query_expansion=request.query_expansion,
            traffic_class=request.traffic_class,
        )
        return RetrievalResponse(
            results=results,
            retrieval_debug=retrieval_debug,
            warnings=warnings,
            grounding=grounding,
        )

    async def health(self) -> dict[str, Any]:
        return {
            "ok": True,
            "impl": "python",
            "capabilities": [
                "search",
                "batch_search",
                "search_with_grounding",
            ],
        }


class PythonScheduler(Scheduler):
    def __init__(
        self,
        *,
        submit_fn: Callable[..., Awaitable[dict[str, Any]]],
        claim_fn: Callable[[str | None], Awaitable[dict[str, Any] | None]],
        update_fn: Callable[..., Awaitable[dict[str, Any] | None]],
        retry_fn: Callable[..., Awaitable[dict[str, Any] | None]],
        metrics_fn: Callable[[], Awaitable[dict[str, Any]]],
    ):
        self._submit_fn = submit_fn
        self._claim_fn = claim_fn
        self._update_fn = update_fn
        self._retry_fn = retry_fn
        self._metrics_fn = metrics_fn

    async def submit_task(self, request: TaskSubmitRequest) -> dict[str, Any]:
        return await self._submit_fn(
            request.title,
            request.project,
            request.agent,
            request.priority,
            request.payload,
            run_after=request.run_after,
            max_attempts=request.max_attempts,
        )

    async def claim_next(self, worker_id: str | None) -> dict[str, Any] | None:
        return await self._claim_fn(worker_id)

    async def update_status(self, request: TaskStatusRequest) -> dict[str, Any] | None:
        return await self._update_fn(
            request.task_id,
            request.status,
            request.message,
            request.metadata,
        )

    async def retry(self, task_id: str, error: str, worker: str) -> dict[str, Any] | None:
        return await self._retry_fn(task_id=task_id, error=error, worker=worker)

    async def queue_metrics(self) -> dict[str, Any]:
        return await self._metrics_fn()


def dataclass_to_dict(value: Any) -> dict[str, Any]:
    if hasattr(value, "__dataclass_fields__"):
        return asdict(value)
    if isinstance(value, dict):
        return dict(value)
    raise TypeError("value is not a dataclass or dict")
