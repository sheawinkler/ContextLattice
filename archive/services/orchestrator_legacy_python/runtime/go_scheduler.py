from __future__ import annotations

import os
from dataclasses import asdict, is_dataclass
from typing import Any

import httpx

from .interfaces import Scheduler, TaskStatusRequest, TaskSubmitRequest


class GoSchedulerProxy(Scheduler):
    """Fail-closed compatibility proxy to the authoritative Gateway Go ledger."""

    def __init__(self, fallback: Scheduler, base_url: str | None = None):
        del fallback  # The compatibility signature remains, but fallback writes are prohibited.
        self._base_url = (
            base_url
            or os.getenv("CONTEXTLATTICE_GATEWAY_URL", "")
            or os.getenv("CONTEXTLATTICE_GO_ORCHESTRATOR_URL", "")
        ).strip().rstrip("/")
        self._api_key = os.getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "").strip()

    def _enabled(self) -> bool:
        return bool(self._base_url)

    async def _post(self, path: str, payload: dict[str, Any], timeout: float = 20.0) -> dict[str, Any]:
        async with httpx.AsyncClient(timeout=timeout) as client:
            resp = await client.post(f"{self._base_url}{path}", json=payload, headers=self._headers())
        if resp.status_code >= 400:
            raise RuntimeError(f"go scheduler failed: status={resp.status_code} body={resp.text[:240]}")
        return resp.json() if resp.content else {}

    def _headers(self) -> dict[str, str]:
        if not self._api_key:
            return {}
        return {"X-Api-Key": self._api_key}

    async def submit_task(self, request: TaskSubmitRequest) -> dict[str, Any]:
        if not self._enabled():
            raise RuntimeError("authoritative Gateway Go task ledger is not configured")
        compatibility = asdict(request) if is_dataclass(request) else dict(request)
        nested = compatibility.get("payload") if isinstance(compatibility.get("payload"), dict) else {}
        manifest = dict(nested.get("manifest")) if isinstance(nested.get("manifest"), dict) else dict(nested)
        for field in (
            "task_id", "project", "workspace_id", "objective", "acceptance_criteria", "task_class",
            "execution_profile", "risk_level", "approval_policy", "context_request", "recipients",
            "review_owner", "requesting_agent_id", "idempotency_key", "priority",
        ):
            if compatibility.get(field) is not None:
                manifest[field] = compatibility[field]
        manifest["objective"] = str(manifest.get("objective") or compatibility.get("title") or "").strip()
        manifest["acceptance_criteria"] = manifest.get("acceptance_criteria") or ["complete the stated objective"]
        review_owner = str(manifest.get("review_owner") or compatibility.get("agent") or "").strip()
        if not review_owner:
            raise RuntimeError("authoritative Gateway Go task ledger requires an explicit review owner")
        manifest["review_owner"] = review_owner
        manifest["requesting_agent_id"] = str(manifest.get("requesting_agent_id") or review_owner).strip()
        manifest["recipients"] = manifest.get("recipients") or [
            {"principal_id": review_owner, "role": "reviewer", "project": manifest.get("project"), "observer": False}
        ]
        manifest["task_class"] = str(manifest.get("task_class") or "non_coding")
        manifest["execution_profile"] = str(manifest.get("execution_profile") or "legacy-compatible")
        manifest["risk_level"] = str(manifest.get("risk_level") or "low")
        manifest["approval_policy"] = manifest.get("approval_policy") or {"required": False}
        context_request = manifest.get("context_request") if isinstance(manifest.get("context_request"), dict) else {}
        if not str(context_request.get("content_hash") or "").startswith("sha256:") or not str(
            context_request.get("session_id") or ""
        ).strip():
            raise RuntimeError("authoritative Gateway Go task ledger requires a pinned context hash and session_id")
        manifest["context_request"] = context_request
        manifest["idempotency_key"] = str(manifest.get("idempotency_key") or f"legacy:{manifest.get('task_id') or compatibility.get('title')}")
        body = await self._post("/agents/tasks", {"manifest": manifest})
        task = body.get("task")
        if not isinstance(task, dict):
            raise RuntimeError("authoritative Gateway Go task ledger returned no task")
        return task

    async def claim_next(self, worker_id: str | None) -> dict[str, Any] | None:
        if not self._enabled():
            raise RuntimeError("authoritative Gateway Go task ledger is not configured")
        worker_instance_id = os.getenv("CONTEXTLATTICE_WORKER_INSTANCE_ID", "").strip()
        if not worker_instance_id:
            raise RuntimeError("authoritative Gateway Go task claim requires CONTEXTLATTICE_WORKER_INSTANCE_ID")
        body = await self._post("/agents/tasks/next", {"worker_id": worker_id, "worker_instance_id": worker_instance_id})
        task = body.get("task")
        if isinstance(task, dict):
            task["_attempt"] = body.get("attempt") if isinstance(body.get("attempt"), dict) else {}
            task["_lease"] = body.get("lease") if isinstance(body.get("lease"), dict) else {}
            return task
        if task is None:
            return task
        raise RuntimeError("authoritative Gateway Go task ledger returned an invalid claim")

    async def update_status(self, request: TaskStatusRequest) -> dict[str, Any] | None:
        if not self._enabled():
            raise RuntimeError("authoritative Gateway Go task ledger is not configured")
        payload = asdict(request) if is_dataclass(request) else dict(request)
        task_id = str(payload.get("task_id", "")).strip()
        if not task_id:
            raise RuntimeError("authoritative Gateway Go task ledger requires task_id")
        payload["runner_status"] = str(payload.get("runner_status") or payload.get("status") or "").strip()
        body = await self._post(f"/agents/tasks/{task_id}/status", payload)
        task = body.get("task")
        if isinstance(task, dict) or task is None:
            return task
        raise RuntimeError("authoritative Gateway Go task ledger returned an invalid status response")

    async def retry(self, task_id: str, error: str, worker: str) -> dict[str, Any] | None:
        del task_id, error, worker
        raise RuntimeError("authoritative Gateway Go retries require lease expiry recovery or an explicit follow-up task")

    async def queue_metrics(self) -> dict[str, Any]:
        if not self._enabled():
            raise RuntimeError("authoritative Gateway Go task ledger is not configured")
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.get(f"{self._base_url}/agents/tasks/runtime", headers=self._headers())
        if resp.status_code >= 400:
            raise RuntimeError(f"authoritative Gateway Go task ledger unavailable: status={resp.status_code}")
        payload = resp.json() if resp.content else {}
        if not isinstance(payload, dict):
            raise RuntimeError("authoritative Gateway Go task ledger returned invalid runtime metrics")
        return payload
