#!/usr/bin/env python3
"""Inference provider routing for local task workers (public lane).

Public v3 lane behavior:
- default provider mode is ``auto``
- on M-series macOS, auto prefers the Ollama CoreML lane
- non-M-series hosts use standard Ollama by default
"""

from __future__ import annotations

from dataclasses import dataclass
import os
import platform
import subprocess
from typing import Optional

import httpx


DEFAULT_OLLAMA_BASE_URL = "http://127.0.0.1:11434"
DEFAULT_LMSTUDIO_BASE_URL = "http://127.0.0.1:1234"
DEFAULT_OPENAI_COMPAT_BASE_URL = "http://127.0.0.1:8000"
DEFAULT_LLAMA_CPP_BASE_URL = "http://127.0.0.1:8080"


@dataclass(frozen=True)
class InferenceRoute:
    requested_provider: str
    provider: str
    transport: str
    base_url: str
    api_key: Optional[str]
    reason: str
    coreml_enabled: bool = False
    sidecar_enabled: bool = False


def _env_bool(name: str, default: bool = False) -> bool:
    raw = str(os.getenv(name, "")).strip().lower()
    if not raw:
        return bool(default)
    return raw in {"1", "true", "yes", "on"}


def _normalize_openai_base(base_url: str) -> str:
    cleaned = str(base_url or "").strip().rstrip("/")
    if not cleaned:
        return cleaned
    if cleaned.endswith("/v1"):
        return cleaned
    return f"{cleaned}/v1"


def _base_url_for_provider(provider: str, override: Optional[str]) -> str:
    if override:
        return override.rstrip("/")
    token = str(provider or "").strip().lower()
    if token in {"ollama", "ollama_coreml"}:
        return DEFAULT_OLLAMA_BASE_URL
    if token == "lmstudio":
        return DEFAULT_LMSTUDIO_BASE_URL
    if token in {"openai-compatible", "openai_compatible", "vllm"}:
        return DEFAULT_OPENAI_COMPAT_BASE_URL
    if token in {"llama-cpp", "llama_cpp"}:
        return DEFAULT_LLAMA_CPP_BASE_URL
    return DEFAULT_OPENAI_COMPAT_BASE_URL


def _is_m_series_mac() -> bool:
    if platform.system().lower() != "darwin":
        return False
    machine = platform.machine().lower()
    if machine not in {"arm64", "aarch64"}:
        return False
    try:
        output = subprocess.check_output(
            ["sysctl", "-n", "machdep.cpu.brand_string"],
            stderr=subprocess.DEVNULL,
            text=True,
            timeout=0.3,
        ).strip()
        if output:
            return "apple" in output.lower()
    except Exception:
        pass
    return True


def _coreml_default_enabled() -> bool:
    return _env_bool("TASK_OLLAMA_COREML_ON_M_SERIES", True)


def _resolve_ollama_route(
    requested_provider: str,
    *,
    base_url_override: Optional[str],
    api_key: Optional[str],
    reason: str,
) -> InferenceRoute:
    coreml_enabled = _coreml_default_enabled() and _is_m_series_mac()
    provider = "ollama_coreml" if coreml_enabled else "ollama"
    return InferenceRoute(
        requested_provider=requested_provider,
        provider=provider,
        transport="ollama",
        base_url=_base_url_for_provider("ollama", base_url_override),
        api_key=api_key,
        reason=reason,
        coreml_enabled=coreml_enabled,
        sidecar_enabled=False,
    )


def resolve_inference_route(
    requested_provider: str,
    *,
    base_url_override: Optional[str] = None,
    api_key: Optional[str] = None,
) -> InferenceRoute:
    requested = str(requested_provider or "").strip().lower()
    provider_mode = str(os.getenv("ORCH_INFER_PROVIDER", "auto")).strip().lower() or "auto"

    if requested in {"", "auto"}:
        requested = provider_mode

    if requested in {"ane", "ane_sidecar"}:
        return _resolve_ollama_route(
            requested,
            base_url_override=base_url_override,
            api_key=api_key,
            reason="ane sidecar is private-lane only; fallback to ollama",
        )

    if requested == "auto":
        return _resolve_ollama_route(
            requested,
            base_url_override=base_url_override,
            api_key=api_key,
            reason="auto provider selected ollama",
        )

    if requested in {"ollama", "ollama_coreml"}:
        return _resolve_ollama_route(
            requested,
            base_url_override=base_url_override,
            api_key=api_key,
            reason="explicit ollama provider",
        )

    if requested in {"lmstudio", "openai-compatible", "openai_compatible", "vllm", "llama-cpp", "llama_cpp"}:
        base_url = _base_url_for_provider(requested, base_url_override)
        return InferenceRoute(
            requested_provider=requested,
            provider=requested,
            transport="openai",
            base_url=base_url,
            api_key=api_key,
            reason="explicit provider",
            coreml_enabled=False,
            sidecar_enabled=False,
        )

    base_url = _base_url_for_provider("openai-compatible", base_url_override)
    return InferenceRoute(
        requested_provider=requested or "unknown",
        provider="openai-compatible",
        transport="openai",
        base_url=base_url,
        api_key=api_key,
        reason=f"unknown provider '{requested}'; defaulted to openai-compatible",
        coreml_enabled=False,
        sidecar_enabled=False,
    )


def _call_openai_compatible(
    base_url: str,
    model: str,
    messages: list[dict[str, str]],
    api_key: Optional[str],
) -> str:
    url = _normalize_openai_base(base_url) + "/chat/completions"
    headers = {"content-type": "application/json"}
    if api_key:
        headers["authorization"] = f"Bearer {api_key}"
    payload = {
        "model": model,
        "messages": messages,
        "temperature": 0.2,
        "stream": False,
    }
    resp = httpx.post(url, json=payload, headers=headers, timeout=60.0)
    resp.raise_for_status()
    data = resp.json()
    return str(((data.get("choices") or [{}])[0].get("message") or {}).get("content") or "")


def _call_ollama(base_url: str, model: str, messages: list[dict[str, str]]) -> str:
    url = base_url.rstrip("/") + "/api/chat"
    payload = {"model": model, "messages": messages, "stream": False}
    resp = httpx.post(url, json=payload, timeout=60.0)
    resp.raise_for_status()
    data = resp.json()
    message = data.get("message") or {}
    return str(message.get("content") or "")


def call_chat_completion(route: InferenceRoute, model: str, messages: list[dict[str, str]]) -> str:
    if route.transport == "ollama":
        return _call_ollama(route.base_url, model, messages)
    return _call_openai_compatible(route.base_url, model, messages, route.api_key)


def format_route_label(route: InferenceRoute) -> str:
    if route.provider == "ollama_coreml":
        return "ollama/coreml"
    return route.provider
