from __future__ import annotations

import hashlib
import importlib
import json
import os
from dataclasses import asdict, is_dataclass
from typing import Any

import httpx

from .interfaces import (
    Codec,
    MemoryStore,
    MemoryWriteRequest,
    RetrievalRequest,
    RetrievalResponse,
    Retriever,
)


def _serialize_payload(value: Any) -> dict[str, Any]:
    if is_dataclass(value):
        return asdict(value)
    if isinstance(value, dict):
        return dict(value)
    raise TypeError("unsupported payload type")


def _engine_auth_headers() -> dict[str, str]:
    headers: dict[str, str] = {}
    api_key = (
        os.getenv("CONTEXTLATTICE_ENGINE_API_KEY", "").strip()
        or os.getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "").strip()
        or os.getenv("MEMMCP_ORCHESTRATOR_API_KEY", "").strip()
    )
    if api_key:
        headers["x-api-key"] = api_key
    return headers


class RustCodecBridge(Codec):
    """Best-effort bridge to future Rust codec bindings with Python fallback."""

    def __init__(self, fallback: Codec, module_name: str | None = None):
        self._fallback = fallback
        self._module_name = module_name or os.getenv("CONTEXTLATTICE_RUST_CODEC_MODULE", "context_codec_py")
        self._module = None
        self._module_error: str | None = None
        try:
            self._module = importlib.import_module(self._module_name)
        except Exception as exc:  # pragma: no cover - exercised in prod only when binding exists
            self._module = None
            self._module_error = str(exc)

    def _dispatch(self, method: str, *args: Any) -> Any:
        if self._module is None:
            return getattr(self._fallback, method)(*args)
        func = getattr(self._module, method, None)
        if func is None:
            return getattr(self._fallback, method)(*args)
        return func(*args)

    def encode_state(self, obj: Any) -> bytes:
        return self._dispatch("encode_state", obj)

    def decode_state(self, payload: bytes) -> Any:
        return self._dispatch("decode_state", payload)

    def encode_batch(self, items: list[Any]) -> bytes:
        return self._dispatch("encode_batch", items)

    def decode_batch(self, payload: bytes) -> list[Any]:
        decoded = self._dispatch("decode_batch", payload)
        if isinstance(decoded, list):
            return decoded
        return [decoded]

    def checksum(self, payload: bytes) -> str:
        value = self._dispatch("checksum", payload)
        if isinstance(value, str) and value:
            return value
        return hashlib.sha256(payload).hexdigest()

    def health(self) -> dict[str, Any]:
        return {
            "module": self._module_name,
            "binding_loaded": self._module is not None,
            "error": self._module_error,
        }


class RustMemoryStoreProxy(MemoryStore):
    """Phase 3/5 proxy for a Rust memory service with Python fallback."""

    def __init__(self, fallback: MemoryStore, base_url: str | None = None):
        self._fallback = fallback
        self._base_url = (base_url or os.getenv("CONTEXTLATTICE_ENGINE_URL", "")).strip().rstrip("/")

    def _enabled(self) -> bool:
        return bool(self._base_url)

    async def _post(self, path: str, payload: dict[str, Any]) -> dict[str, Any]:
        async with httpx.AsyncClient(timeout=30.0) as client:
            resp = await client.post(
                f"{self._base_url}{path}",
                json=payload,
                headers=_engine_auth_headers(),
            )
        if resp.status_code >= 400:
            raise RuntimeError(f"rust memory service failed: status={resp.status_code} body={resp.text[:240]}")
        return resp.json() if resp.content else {}

    async def add_memory(self, payload: MemoryWriteRequest) -> str:
        if not self._enabled():
            return await self._fallback.add_memory(payload)
        try:
            body = await self._post("/v1/memory/put", {"item": _serialize_payload(payload)})
            memory_id = str(body.get("memory_id") or "").strip()
            if memory_id:
                return memory_id
        except Exception:
            pass
        return await self._fallback.add_memory(payload)

    async def update_memory(self, memory_id: str, patch: dict[str, Any]) -> bool:
        if not self._enabled():
            return await self._fallback.update_memory(memory_id, patch)
        try:
            await self._post("/v1/memory/update", {"memory_id": memory_id, "patch": patch})
            return True
        except Exception:
            return await self._fallback.update_memory(memory_id, patch)

    async def get_memory(self, memory_id: str) -> dict[str, Any] | None:
        if not self._enabled():
            return await self._fallback.get_memory(memory_id)
        try:
            async with httpx.AsyncClient(timeout=20.0) as client:
                resp = await client.get(
                    f"{self._base_url}/v1/memory/get",
                    params={"memory_id": memory_id},
                    headers=_engine_auth_headers(),
                )
            if resp.status_code >= 400:
                raise RuntimeError(resp.text)
            payload = resp.json() if resp.content else {}
            memory = payload.get("memory")
            if isinstance(memory, dict):
                return memory
        except Exception:
            return await self._fallback.get_memory(memory_id)
        return None

    async def query_neighbors(
        self,
        memory_id: str,
        filters: dict[str, Any] | None = None,
        limit: int = 10,
    ) -> list[dict[str, Any]]:
        if not self._enabled():
            return await self._fallback.query_neighbors(memory_id, filters=filters, limit=limit)
        try:
            payload = await self._post(
                "/v1/memory/neighbors",
                {
                    "memory_id": memory_id,
                    "filters": filters or {},
                    "limit": max(1, int(limit)),
                },
            )
            rows = payload.get("results")
            if isinstance(rows, list):
                return [row for row in rows if isinstance(row, dict)]
        except Exception:
            return await self._fallback.query_neighbors(memory_id, filters=filters, limit=limit)
        return []

    async def batch_insert(self, items: list[MemoryWriteRequest]) -> list[str]:
        if not self._enabled():
            return await self._fallback.batch_insert(items)
        try:
            payload = await self._post(
                "/v1/memory/batch-put",
                {"items": [_serialize_payload(item) for item in items]},
            )
            ids = payload.get("memory_ids")
            if isinstance(ids, list):
                return [str(item) for item in ids if str(item).strip()]
        except Exception:
            return await self._fallback.batch_insert(items)
        return []


class RustRetrieverProxy(Retriever):
    """Phase 4/5 proxy for a Rust retrieval service with Python fallback."""

    def __init__(self, fallback: Retriever, base_url: str | None = None):
        self._fallback = fallback
        self._base_url = (base_url or os.getenv("CONTEXTLATTICE_ENGINE_URL", "")).strip().rstrip("/")

    def _enabled(self) -> bool:
        return bool(self._base_url)

    async def _post(self, path: str, payload: dict[str, Any], timeout: float = 45.0) -> dict[str, Any]:
        async with httpx.AsyncClient(timeout=timeout) as client:
            resp = await client.post(
                f"{self._base_url}{path}",
                json=payload,
                headers=_engine_auth_headers(),
            )
        if resp.status_code >= 400:
            raise RuntimeError(f"rust retrieval service failed: status={resp.status_code} body={resp.text[:240]}")
        return resp.json() if resp.content else {}

    async def search(self, request: RetrievalRequest) -> list[dict[str, Any]]:
        if not self._enabled():
            return await self._fallback.search(request)
        try:
            payload = await self._post("/v1/retrieval/query", {"request": _serialize_payload(request)})
            rows = payload.get("results")
            if isinstance(rows, list):
                return [row for row in rows if isinstance(row, dict)]
        except Exception:
            return await self._fallback.search(request)
        return []

    async def batch_search(self, requests: list[RetrievalRequest]) -> list[list[dict[str, Any]]]:
        if not self._enabled():
            return await self._fallback.batch_search(requests)
        try:
            payload = await self._post(
                "/v1/retrieval/batch-query",
                {"requests": [_serialize_payload(request) for request in requests]},
            )
            batches = payload.get("results")
            if not isinstance(batches, list):
                raise RuntimeError("invalid batch response")
            parsed: list[list[dict[str, Any]]] = []
            for batch in batches:
                if not isinstance(batch, list):
                    parsed.append([])
                    continue
                parsed.append([row for row in batch if isinstance(row, dict)])
            return parsed
        except Exception:
            return await self._fallback.batch_search(requests)

    async def search_with_grounding(self, request: RetrievalRequest) -> RetrievalResponse:
        if not self._enabled():
            return await self._fallback.search_with_grounding(request)
        try:
            payload = await self._post(
                "/v1/retrieval/query-with-grounding",
                {"request": _serialize_payload(request)},
            )
            response = RetrievalResponse(
                results=payload.get("results") if isinstance(payload.get("results"), list) else [],
                retrieval_debug=payload.get("retrieval_debug")
                if isinstance(payload.get("retrieval_debug"), dict)
                else {},
                warnings=payload.get("warnings") if isinstance(payload.get("warnings"), list) else [],
                grounding=payload.get("grounding") if isinstance(payload.get("grounding"), dict) else {},
            )
            return response
        except Exception:
            return await self._fallback.search_with_grounding(request)

    async def health(self) -> dict[str, Any]:
        if not self._enabled():
            fallback_health = await self._fallback.health()
            fallback_health["proxy"] = "disabled"
            fallback_health["base_url"] = self._base_url
            return fallback_health
        try:
            async with httpx.AsyncClient(timeout=10.0) as client:
                resp = await client.get(
                    f"{self._base_url}/v1/retrieval/health",
                    headers=_engine_auth_headers(),
                )
            if resp.status_code >= 400:
                raise RuntimeError(resp.text)
            payload = resp.json() if resp.content else {}
            payload["proxy"] = "rust"
            payload["base_url"] = self._base_url
            return payload
        except Exception as exc:
            fallback_health = await self._fallback.health()
            fallback_health["proxy"] = "fallback"
            fallback_health["base_url"] = self._base_url
            fallback_health["error"] = str(exc)
            return fallback_health


def checksum_for_payload(payload: dict[str, Any]) -> str:
    data = json.dumps(payload, ensure_ascii=True, sort_keys=True).encode("utf-8")
    return hashlib.sha256(data).hexdigest()
