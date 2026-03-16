from __future__ import annotations

from dataclasses import asdict, dataclass
from typing import Any, Awaitable, Callable

from .flags import MigrationFlags
from .go_scheduler import GoSchedulerProxy
from .interfaces import Codec, MemoryStore, Retriever, Scheduler, StateDelta
from .python_impl import JsonCodec, JsonMergeStateDelta, PythonMemoryStore, PythonRetriever, PythonScheduler
from .rust_stub import RustCodecBridge, RustMemoryStoreProxy, RustRetrieverProxy


@dataclass(slots=True)
class RuntimeCallbacks:
    write_memory_fn: Callable[..., Awaitable[dict[str, Any]]]
    read_memory_fn: Callable[..., Awaitable[str]]
    list_files_fn: Callable[..., Awaitable[list[str]]]
    search_neighbors_fn: Callable[..., Awaitable[list[dict[str, Any]]]] | None
    federated_search_fn: Callable[..., Awaitable[tuple[list[dict[str, Any]], dict[str, Any], list[str]]]]
    recall_pipeline_fn: Callable[..., Awaitable[tuple[list[dict[str, Any]], dict[str, Any], list[str], dict[str, Any]]]]
    submit_task_fn: Callable[..., Awaitable[dict[str, Any]]]
    claim_task_fn: Callable[..., Awaitable[dict[str, Any] | None]]
    update_task_fn: Callable[..., Awaitable[dict[str, Any] | None]]
    retry_task_fn: Callable[..., Awaitable[dict[str, Any] | None]]
    scheduler_metrics_fn: Callable[..., Awaitable[dict[str, Any]]]


@dataclass(slots=True)
class MigrationRuntime:
    flags: MigrationFlags
    codec: Codec
    memory_store: MemoryStore
    retriever: Retriever
    scheduler: Scheduler
    state_delta: StateDelta
    implementation_map: dict[str, str]



def build_runtime(flags: MigrationFlags, callbacks: RuntimeCallbacks) -> MigrationRuntime:
    base_codec = JsonCodec()
    codec: Codec = base_codec
    if flags.use_rust_codec:
        codec = RustCodecBridge(base_codec)

    state_delta: StateDelta = JsonMergeStateDelta()

    python_memory = PythonMemoryStore(
        write_memory_fn=callbacks.write_memory_fn,
        read_memory_fn=callbacks.read_memory_fn,
        list_files_fn=callbacks.list_files_fn,
        search_neighbors_fn=callbacks.search_neighbors_fn,
    )
    memory_store: MemoryStore = python_memory
    if flags.use_rust_memory:
        memory_store = RustMemoryStoreProxy(python_memory)

    python_retriever = PythonRetriever(
        federated_search_fn=callbacks.federated_search_fn,
        recall_pipeline_fn=callbacks.recall_pipeline_fn,
    )
    retriever: Retriever = python_retriever
    if flags.use_rust_retrieval:
        retriever = RustRetrieverProxy(python_retriever)

    python_scheduler = PythonScheduler(
        submit_fn=callbacks.submit_task_fn,
        claim_fn=callbacks.claim_task_fn,
        update_fn=callbacks.update_task_fn,
        retry_fn=callbacks.retry_task_fn,
        metrics_fn=callbacks.scheduler_metrics_fn,
    )
    scheduler: Scheduler = python_scheduler
    if flags.use_go_orchestrator:
        scheduler = GoSchedulerProxy(python_scheduler)

    implementation_map = {
        "codec": type(codec).__name__,
        "memory_store": type(memory_store).__name__,
        "retriever": type(retriever).__name__,
        "scheduler": type(scheduler).__name__,
        "state_delta": type(state_delta).__name__,
    }

    return MigrationRuntime(
        flags=flags,
        codec=codec,
        memory_store=memory_store,
        retriever=retriever,
        scheduler=scheduler,
        state_delta=state_delta,
        implementation_map=implementation_map,
    )


async def runtime_snapshot(runtime: MigrationRuntime) -> dict[str, Any]:
    retriever_health = await runtime.retriever.health()
    return {
        "flags": asdict(runtime.flags),
        "implementations": dict(runtime.implementation_map),
        "retriever_health": retriever_health,
    }
