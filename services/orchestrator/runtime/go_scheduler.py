from __future__ import annotations

import os
from dataclasses import asdict, is_dataclass
from typing import Any

import httpx

from .interfaces import Scheduler, TaskStatusRequest, TaskSubmitRequest


class GoSchedulerProxy(Scheduler):
    """Phase 6 proxy to a Go scheduler service with Python fallback."""

    def __init__(self, fallback: Scheduler, base_url: str | None = None):
        self._fallback = fallback
        self._base_url = (base_url or os.getenv("CONTEXTLATTICE_GO_ORCHESTRATOR_URL", "")).strip().rstrip("/")

    def _enabled(self) -> bool:
        return bool(self._base_url)

    async def _post(self, path: str, payload: dict[str, Any], timeout: float = 20.0) -> dict[str, Any]:
        async with httpx.AsyncClient(timeout=timeout) as client:
            resp = await client.post(f"{self._base_url}{path}", json=payload)
        if resp.status_code >= 400:
            raise RuntimeError(f"go scheduler failed: status={resp.status_code} body={resp.text[:240]}")
        return resp.json() if resp.content else {}

    async def submit_task(self, request: TaskSubmitRequest) -> dict[str, Any]:
        if not self._enabled():
            return await self._fallback.submit_task(request)
        try:
            payload = asdict(request) if is_dataclass(request) else dict(request)
            body = await self._post("/v1/tasks/submit", payload)
            task = body.get("task")
            if isinstance(task, dict):
                return task
        except Exception:
            return await self._fallback.submit_task(request)
        return {}

    async def claim_next(self, worker_id: str | None) -> dict[str, Any] | None:
        if not self._enabled():
            return await self._fallback.claim_next(worker_id)
        try:
            body = await self._post("/v1/tasks/claim", {"worker_id": worker_id})
            task = body.get("task")
            if isinstance(task, dict) or task is None:
                return task
        except Exception:
            return await self._fallback.claim_next(worker_id)
        return None

    async def update_status(self, request: TaskStatusRequest) -> dict[str, Any] | None:
        if not self._enabled():
            return await self._fallback.update_status(request)
        try:
            payload = asdict(request) if is_dataclass(request) else dict(request)
            body = await self._post("/v1/tasks/status", payload)
            task = body.get("task")
            if isinstance(task, dict) or task is None:
                return task
        except Exception:
            return await self._fallback.update_status(request)
        return None

    async def retry(self, task_id: str, error: str, worker: str) -> dict[str, Any] | None:
        if not self._enabled():
            return await self._fallback.retry(task_id, error, worker)
        try:
            body = await self._post(
                "/v1/tasks/retry",
                {"task_id": task_id, "error": error, "worker": worker},
            )
            task = body.get("task")
            if isinstance(task, dict) or task is None:
                return task
        except Exception:
            return await self._fallback.retry(task_id, error, worker)
        return None

    async def queue_metrics(self) -> dict[str, Any]:
        if not self._enabled():
            return await self._fallback.queue_metrics()
        try:
            async with httpx.AsyncClient(timeout=10.0) as client:
                resp = await client.get(f"{self._base_url}/v1/tasks/metrics")
            if resp.status_code >= 400:
                raise RuntimeError(resp.text)
            payload = resp.json() if resp.content else {}
            if isinstance(payload, dict):
                return payload
        except Exception:
            return await self._fallback.queue_metrics()
        return {}
