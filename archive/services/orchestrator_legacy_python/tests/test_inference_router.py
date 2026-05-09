from __future__ import annotations

from pathlib import Path
import sys

import pytest


REPO_ROOT = Path(__file__).resolve().parents[3]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from scripts import inference_router as ir


def test_auto_route_prefers_ollama_coreml_on_m_series(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("ORCH_INFER_PROVIDER", "auto")
    monkeypatch.setenv("TASK_OLLAMA_COREML_ON_M_SERIES", "true")
    monkeypatch.setattr(ir, "_is_m_series_mac", lambda: True)

    route = ir.resolve_inference_route("auto")

    assert route.provider == "ollama_coreml"
    assert route.transport == "ollama"
    assert route.coreml_enabled is True


def test_auto_route_uses_plain_ollama_on_non_m_series(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("ORCH_INFER_PROVIDER", "auto")
    monkeypatch.setenv("TASK_OLLAMA_COREML_ON_M_SERIES", "true")
    monkeypatch.setattr(ir, "_is_m_series_mac", lambda: False)

    route = ir.resolve_inference_route("auto")

    assert route.provider == "ollama"
    assert route.transport == "ollama"
    assert route.coreml_enabled is False


def test_ane_request_falls_back_to_ollama_in_public_lane(monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("TASK_OLLAMA_COREML_ON_M_SERIES", "false")
    monkeypatch.setattr(ir, "_is_m_series_mac", lambda: True)

    route = ir.resolve_inference_route("ane_sidecar")

    assert route.provider == "ollama"
    assert "fallback to ollama" in route.reason
