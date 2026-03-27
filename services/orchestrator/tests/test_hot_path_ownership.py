from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

import pytest


def _load_orchestrator_module():
    app_path = Path(__file__).resolve().parents[1] / "app.py"
    spec = importlib.util.spec_from_file_location("orchestrator_app_hot_path_test", app_path)
    if spec is None or spec.loader is None:
        raise RuntimeError("Unable to load orchestrator app module")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


orchestrator = _load_orchestrator_module()


def _hot_route_pairs() -> set[tuple[str, str]]:
    rows: set[tuple[str, str]] = set()
    for route in orchestrator.app.routes:
        path = getattr(route, "path", "")
        methods = getattr(route, "methods", set()) or set()
        if not path:
            continue
        if not orchestrator._is_python_hot_path(path):
            continue
        for method in methods:
            if method in {"HEAD", "OPTIONS"}:
                continue
            rows.add((method, path))
    return rows


def test_python_hot_path_routes_are_explicitly_allowlisted() -> None:
    expected = {
        ("GET", "/memory/search/async/{token}"),
        ("GET", "/memory/search/continuations/{token}"),
        ("GET", "/memory/search/continuations/{token}/events"),
        ("GET", "/memory/search/jobs/{job_id}"),
        ("GET", "/memory/search/jobs/{job_id}/events"),
        ("POST", "/memory/context-pack"),
        ("POST", "/memory/search"),
        ("POST", "/memory/write"),
        ("POST", "/memory/write/batch"),
        ("POST", "/v1/memory/batch-put"),
        ("POST", "/v1/memory/put"),
        ("POST", "/v1/retrieval/batch-query"),
        ("POST", "/v1/retrieval/query"),
        ("POST", "/v1/retrieval/query-with-grounding"),
    }
    actual = _hot_route_pairs()
    assert actual == expected, (
        "Python hot-path route set changed. "
        "Add Go/Rust ownership first, then update this allowlist intentionally. "
        f"missing={sorted(expected - actual)} extra={sorted(actual - expected)}"
    )


@pytest.mark.asyncio
async def test_python_hot_path_ownership_payload_flags_non_gateway_requests(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(orchestrator, "python_hot_path_non_gateway_total", 0)
    monkeypatch.setattr(orchestrator, "python_hot_path_non_gateway_by_path", {})
    monkeypatch.setattr(orchestrator, "python_hot_path_last_non_gateway_at", None)

    await orchestrator._record_python_hot_path_non_gateway("/memory/search")
    payload = orchestrator._python_hot_path_ownership_payload()

    assert payload["ok"] is False
    assert payload["status"] == "non_gateway_hot_path_traffic_detected"
    assert payload["nonGatewayRequests"] == 1
    assert payload["byPath"].get("/memory/search") == 1
    assert payload["lastNonGatewayAt"]
