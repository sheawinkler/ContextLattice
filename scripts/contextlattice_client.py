#!/usr/bin/env python3
"""
Shared ContextLattice client helper.

This module centralizes orchestrator request logic (base URL, API key resolution,
headers, timeouts) so scripts do not duplicate drift-prone request code.
"""

from __future__ import annotations

import json as jsonlib
import os
import urllib.error
import urllib.request
from urllib.parse import urlencode, urljoin
from typing import Any, Mapping

try:
    import httpx
except ModuleNotFoundError:  # pragma: no cover - exercised in dependency-free CI.
    httpx = None

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


def build_orchestrator_headers(
    api_key: str | None = None,
    extra_headers: Mapping[str, str] | None = None,
) -> dict[str, str]:
    headers: dict[str, str] = {}
    key = str(api_key or "").strip()
    if key:
        headers["x-api-key"] = key
    if extra_headers:
        for name, value in extra_headers.items():
            header_name = str(name or "").strip()
            header_value = str(value or "").strip()
            if header_name and header_value:
                headers[header_name] = header_value
    return headers


class _UrllibResponse:
    def __init__(self, status_code: int, headers: dict[str, str], body: bytes) -> None:
        self.status_code = status_code
        self.headers = headers
        self._body = body

    @property
    def text(self) -> str:
        return self._body.decode("utf-8", errors="replace")

    def json(self) -> Any:
        return jsonlib.loads(self.text or "{}")

    def raise_for_status(self) -> None:
        if self.status_code >= 400:
            raise RuntimeError(f"ContextLattice request failed status={self.status_code}: {self.text[:500]}")


class _UrllibClient:
    def __init__(self, *, timeout: float, headers: dict[str, str]) -> None:
        self.timeout = max(1.0, float(timeout))
        self.headers = dict(headers)

    def close(self) -> None:
        return None

    def _request(
        self,
        method: str,
        url: str,
        *,
        params: dict[str, str] | None = None,
        json: dict[str, Any] | None = None,
        timeout: float | None = None,
    ) -> _UrllibResponse:
        if params:
            separator = "&" if "?" in url else "?"
            url = url + separator + urlencode(params)
        headers = dict(self.headers)
        data = None
        if json is not None:
            data = jsonlib.dumps(json).encode("utf-8")
            headers["content-type"] = "application/json"
        request = urllib.request.Request(url, data=data, method=method.upper(), headers=headers)
        try:
            with urllib.request.urlopen(request, timeout=timeout or self.timeout) as response:
                return _UrllibResponse(response.status, dict(response.headers.items()), response.read())
        except urllib.error.HTTPError as exc:
            return _UrllibResponse(exc.code, dict(exc.headers.items()), exc.read())

    def get(self, url: str, *, params: dict[str, str] | None = None, timeout: float | None = None) -> _UrllibResponse:
        return self._request("GET", url, params=params, timeout=timeout)

    def post(
        self,
        url: str,
        *,
        json: dict[str, Any] | None = None,
        params: dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> _UrllibResponse:
        return self._request("POST", url, params=params, json=json, timeout=timeout)


class ContextLatticeClient:
    """Small HTTP client wrapper for ContextLattice orchestrator calls."""

    def __init__(
        self,
        base_url: str | None = None,
        *,
        timeout: float = 30.0,
        role: str = "orchestrator",
        api_key: str | None = None,
        extra_headers: Mapping[str, str] | None = None,
    ) -> None:
        self.base_url = str(base_url or DEFAULT_ORCHESTRATOR_URL).rstrip("/")
        resolved_key = (
            str(api_key).strip()
            if api_key is not None
            else resolve_orchestrator_api_key(role=role)
        )
        headers = build_orchestrator_headers(resolved_key, extra_headers)
        self.last_response_headers: dict[str, str] = {}
        self.last_response_status: int | None = None
        self.last_error_code: str = ""
        if httpx is not None:
            self.client = httpx.Client(
                timeout=max(1.0, float(timeout)),
                headers=headers,
            )
        else:
            self.client = _UrllibClient(timeout=timeout, headers=headers)

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
        self.last_response_status = int(response.status_code)
        self.last_response_headers = {
            str(key).lower(): str(value) for key, value in response.headers.items()
        }
        self.last_error_code = ""
        if self.last_response_status >= 400:
            try:
                error_payload = response.json()
            except Exception:
                error_payload = None
            if isinstance(error_payload, dict):
                self.last_error_code = str(error_payload.get("code") or "").strip()
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
        self.last_response_status = int(response.status_code)
        self.last_response_headers = {
            str(key).lower(): str(value) for key, value in response.headers.items()
        }
        self.last_error_code = ""
        if self.last_response_status >= 400:
            try:
                error_payload = response.json()
            except Exception:
                error_payload = None
            if isinstance(error_payload, dict):
                self.last_error_code = str(error_payload.get("code") or "").strip()
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
