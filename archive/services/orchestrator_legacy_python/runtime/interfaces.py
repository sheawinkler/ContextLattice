from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Protocol


@dataclass(slots=True)
class RetrievalRequest:
    query: str
    limit: int = 10
    project_filter: str | None = None
    topic_filter: str | None = None
    sources: list[str] | None = None
    source_weights: dict[str, float] | None = None
    preferences: dict[str, Any] | None = None
    rerank_with_learning: bool = True
    retrieval_mode: str = "balanced"
    retrieval_intent: str = "decision"
    agent_profile: dict[str, Any] | None = None
    auto_escalate: bool = False
    query_expansion: bool = True
    traffic_class: str = "user"
    backend_policy: dict[str, Any] | None = None


@dataclass(slots=True)
class RetrievalResponse:
    results: list[dict[str, Any]] = field(default_factory=list)
    retrieval_debug: dict[str, Any] = field(default_factory=dict)
    warnings: list[str] = field(default_factory=list)
    grounding: dict[str, Any] = field(default_factory=dict)


@dataclass(slots=True)
class TaskSubmitRequest:
    title: str
    project: str | None
    agent: str | None
    priority: int
    payload: dict[str, Any] | None
    run_after: str | None = None
    max_attempts: int | None = None


@dataclass(slots=True)
class TaskStatusRequest:
    task_id: str
    status: str
    message: str | None = None
    metadata: dict[str, Any] | None = None


@dataclass(slots=True)
class MemoryWriteRequest:
    project: str
    file_name: str
    content: str
    topic_path: str | None = None


class Codec(Protocol):
    def encode_state(self, obj: Any) -> bytes: ...

    def decode_state(self, payload: bytes) -> Any: ...

    def encode_batch(self, items: list[Any]) -> bytes: ...

    def decode_batch(self, payload: bytes) -> list[Any]: ...

    def checksum(self, payload: bytes) -> str: ...


class MemoryStore(Protocol):
    async def add_memory(self, payload: MemoryWriteRequest) -> str: ...

    async def update_memory(self, memory_id: str, patch: dict[str, Any]) -> bool: ...

    async def get_memory(self, memory_id: str) -> dict[str, Any] | None: ...

    async def query_neighbors(
        self,
        memory_id: str,
        filters: dict[str, Any] | None = None,
        limit: int = 10,
    ) -> list[dict[str, Any]]: ...

    async def batch_insert(self, items: list[MemoryWriteRequest]) -> list[str]: ...


class Retriever(Protocol):
    async def search(self, request: RetrievalRequest) -> list[dict[str, Any]]: ...

    async def batch_search(
        self,
        requests: list[RetrievalRequest],
    ) -> list[list[dict[str, Any]]]: ...

    async def search_with_grounding(self, request: RetrievalRequest) -> RetrievalResponse: ...

    async def health(self) -> dict[str, Any]: ...


class Scheduler(Protocol):
    async def submit_task(self, request: TaskSubmitRequest) -> dict[str, Any]: ...

    async def claim_next(self, worker_id: str | None) -> dict[str, Any] | None: ...

    async def update_status(self, request: TaskStatusRequest) -> dict[str, Any] | None: ...

    async def retry(self, task_id: str, error: str, worker: str) -> dict[str, Any] | None: ...

    async def queue_metrics(self) -> dict[str, Any]: ...


class StateDelta(Protocol):
    def diff(self, old_state: dict[str, Any], new_state: dict[str, Any]) -> dict[str, Any]: ...

    def apply(self, state: dict[str, Any], delta: dict[str, Any]) -> dict[str, Any]: ...

    def compose(self, delta_a: dict[str, Any], delta_b: dict[str, Any]) -> dict[str, Any]: ...

    def validate(self, delta: dict[str, Any]) -> bool: ...
