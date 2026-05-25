#!/usr/bin/env python3
"""Synthetic transport benchmark for ANE sidecar routing.

This benchmark measures request-path overhead only using mock local HTTP endpoints.
It does not represent real model inference quality or hardware acceleration gains.
"""

from __future__ import annotations

import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import sys
import statistics
import threading
import time
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[1]
if str(REPO_ROOT) not in sys.path:
    sys.path.insert(0, str(REPO_ROOT))

from scripts.inference_router import call_chat_completion, resolve_inference_route


class _MockHandler(BaseHTTPRequestHandler):
    mode = "sidecar"

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A003
        return

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/health":
            payload = {"healthy": True, "detail": f"mock-{self.mode}"}
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(payload).encode("utf-8"))
            return
        self.send_response(404)
        self.end_headers()

    def do_POST(self) -> None:  # noqa: N802
        content_length = int(self.headers.get("Content-Length", "0") or "0")
        if content_length > 0:
            _ = self.rfile.read(content_length)

        if self.mode == "sidecar" and self.path == "/v1/chat/completions":
            payload = {
                "choices": [
                    {
                        "message": {
                            "role": "assistant",
                            "content": "ok-sidecar",
                        }
                    }
                ]
            }
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(payload).encode("utf-8"))
            return

        if self.mode == "ollama" and self.path == "/api/chat":
            payload = {"message": {"role": "assistant", "content": "ok-ollama"}}
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(payload).encode("utf-8"))
            return

        self.send_response(404)
        self.end_headers()


def _start_server(mode: str, port: int) -> ThreadingHTTPServer:
    handler = type(f"_{mode.capitalize()}Handler", (_MockHandler,), {"mode": mode})
    server = ThreadingHTTPServer(("127.0.0.1", port), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def _percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    if len(values) == 1:
        return float(values[0])
    ordered = sorted(values)
    idx = (len(ordered) - 1) * pct
    low = int(idx)
    high = min(len(ordered) - 1, low + 1)
    weight = idx - low
    return (ordered[low] * (1.0 - weight)) + (ordered[high] * weight)


def _run_calls(route_provider: str, runs: int, base_url: str | None = None) -> dict[str, Any]:
    latencies: list[float] = []
    errors = 0
    route = resolve_inference_route(route_provider, base_url_override=base_url)
    for _ in range(max(1, runs)):
        started = time.perf_counter()
        try:
            _ = call_chat_completion(
                route,
                "qwen3.5:9b",
                [{"role": "user", "content": "ping"}],
            )
        except Exception:
            errors += 1
        latencies.append((time.perf_counter() - started) * 1000.0)

    return {
        "runs": len(latencies),
        "errors": errors,
        "latencyMs": {
            "p50": round(_percentile(latencies, 0.50), 3),
            "p95": round(_percentile(latencies, 0.95), 3),
            "mean": round(statistics.mean(latencies), 3),
            "min": round(min(latencies), 3),
            "max": round(max(latencies), 3),
        },
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Synthetic ANE sidecar route benchmark")
    parser.add_argument("--runs", type=int, default=40)
    parser.add_argument("--sidecar-port", type=int, default=19099)
    parser.add_argument("--ollama-port", type=int, default=19134)
    parser.add_argument("--output", default="")
    args = parser.parse_args()

    sidecar_server = _start_server("sidecar", args.sidecar_port)
    ollama_server = _start_server("ollama", args.ollama_port)

    prev_env = dict(os.environ)
    try:
        os.environ["TASK_OLLAMA_COREML_ON_M_SERIES"] = "false"
        os.environ["ORCH_ANE_SIDECAR_ENABLED"] = "true"
        os.environ["ORCH_ANE_SIDECAR_REQUIRE_M_SERIES"] = "false"
        os.environ["ORCH_ANE_SIDECAR_FALLBACK_ENABLED"] = "true"
        os.environ["ORCH_ANE_SIDECAR_URL"] = f"http://127.0.0.1:{args.sidecar_port}"
        os.environ["ORCH_ANE_SIDECAR_HEALTH_URL"] = f"http://127.0.0.1:{args.sidecar_port}/health"
        os.environ["ORCH_INFER_PROVIDER"] = "ane_sidecar"

        sidecar_metrics = _run_calls("ane_sidecar", args.runs)

        os.environ["ORCH_ANE_SIDECAR_ENABLED"] = "false"
        os.environ["ORCH_INFER_PROVIDER"] = "auto"
        ollama_metrics = _run_calls("ollama", args.runs, base_url=f"http://127.0.0.1:{args.ollama_port}")

        report = {
            "generatedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "runs": args.runs,
            "note": "Synthetic mock benchmark; request path overhead only.",
            "ane_sidecar": sidecar_metrics,
            "ollama": ollama_metrics,
        }
        if args.output:
            path = Path(args.output)
        else:
            stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
            path = Path(f"bench/results/ane_sidecar_route_bench_{stamp}.json")
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(report, indent=2), encoding="utf-8")
        print(json.dumps(report, indent=2))
        print(f"OUTPUT={path}")
    finally:
        os.environ.clear()
        os.environ.update(prev_env)
        sidecar_server.shutdown()
        ollama_server.shutdown()

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
