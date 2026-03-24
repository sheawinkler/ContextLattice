#!/usr/bin/env python3
"""
Shared ContextLattice client helper.

This module centralizes orchestrator request logic (base URL, API key resolution,
headers, timeouts) so scripts do not duplicate drift-prone request code.
"""

from __future__ import annotations

import os
from urllib.parse import urljoin
from typing import Any

import httpx

DEFAULT_ORCHESTRATOR_URL = os.getenv(
    "CONTEXTLATTICE_ORCHESTRATOR_URL",
    os.getenv("CONTEXTLATTICE_ORCHESTRATOR_URL", "http://127.0.0.1:8075"),
)


def resolve_orchestrator_api_key(role: str = "orchestrator") -> str:
    """
    Resolve API key for caller role.

    - orchestrator role: CONTEXTLATTICE_ORCHESTRATOR_API_KEY | CONTEXTLATTICE_ORCHESTRATOR_API_KEY
    - worker role: CONTEXTLATTICE_WORKER_API_KEY | CONTEXTLATTICE_WORKER_API_KEY, then falls
      back to orchestrator key for compatibility when dedicated worker key is unset.
    """
    role_token = str(role or "").strip().lower()
    if role_token == "worker":
        worker_key = (
            str(os.getenv("CONTEXTLATTICE_WORKER_API_KEY") or "").strip()
            or str(os.getenv("CONTEXTLATTICE_WORKER_API_KEY") or "").strip()
        )
        if worker_key:
            return worker_key
    return (
        str(os.getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY") or "").strip()
        or str(os.getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY") or "").strip()
    )


def build_orchestrator_headers(api_key: str | None = None) -> dict[str, str]:
    headers: dict[str, str] = {}
    key = str(api_key or "").strip()
    if key:
        headers["x-api-key"] = key
    return headers


class ContextLatticeClient:
    """Small HTTP client wrapper for ContextLattice orchestrator calls."""

    def __init__(
        self,
        base_url: str | None = None,
        *,
        timeout: float = 30.0,
        role: str = "orchestrator",
        api_key: str | None = None,
    ) -> None:
        self.base_url = str(base_url or DEFAULT_ORCHESTRATOR_URL).rstrip("/")
        resolved_key = (
            str(api_key).strip()
            if api_key is not None
            else resolve_orchestrator_api_key(role=role)
        )
        self.client = httpx.Client(
            timeout=max(1.0, float(timeout)),
            headers=build_orchestrator_headers(resolved_key),
        )

    def close(self) -> None:
        self.client.close()

    def _absolute_url(self, path_or_url: str) -> str:
        token = str(path_or_url or "").strip()
        if token.startswith("http://") or token.startswith("https://"):
            return token
        if not token.startswith("/"):
            token = "/" + token
        return urljoin(self.base_url + "/", token.lstrip("/"))

    def get_json(
        self,
        path_or_url: str,
        *,
        params: dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> dict[str, Any]:
        url = self._absolute_url(path_or_url)
        response = self.client.get(url, params=params, timeout=timeout)
        response.raise_for_status()
        payload = response.json()
        return payload if isinstance(payload, dict) else {"data": payload}

    def post_json(
        self,
        path_or_url: str,
        payload: dict[str, Any],
        *,
        params: dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> dict[str, Any]:
        url = self._absolute_url(path_or_url)
        response = self.client.post(url, json=payload, params=params, timeout=timeout)
        response.raise_for_status()
        body = response.json()
        return body if isinstance(body, dict) else {"data": body}


def create_orchestrator_client(
    base_url: str | None = None,
    *,
    timeout: float = 30.0,
    api_key: str | None = None,
) -> ContextLatticeClient:
    return ContextLatticeClient(
        base_url=base_url,
        timeout=timeout,
        role="orchestrator",
        api_key=api_key,
    )


def create_worker_client(
    base_url: str | None = None,
    *,
    timeout: float = 30.0,
    api_key: str | None = None,
) -> ContextLatticeClient:
    return ContextLatticeClient(
        base_url=base_url,
        timeout=timeout,
        role="worker",
        api_key=api_key,
    )

