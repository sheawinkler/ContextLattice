#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import statistics
import time
from datetime import datetime, timezone
from pathlib import Path

import httpx


def percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    if len(values) == 1:
        return float(values[0])
    index = (len(values) - 1) * pct
    low = int(index)
    high = min(len(values) - 1, low + 1)
    weight = index - low
    return (values[low] * (1 - weight)) + (values[high] * weight)


def run(args: argparse.Namespace) -> dict:
    headers = {"content-type": "application/json"}
    if args.api_key:
        headers["x-api-key"] = args.api_key

    timings: list[float] = []
    errors: list[str] = []

    with httpx.Client(timeout=args.timeout) as client:
        runtime_resp = client.get(f"{args.base_url.rstrip('/')}/migration/runtime", headers=headers)
        runtime_payload = runtime_resp.json() if runtime_resp.content else {}

        for idx in range(args.requests):
            body = {
                "query": f"runtime benchmark query {idx % 3}",
                "limit": 8,
                "include_retrieval_debug": True,
                "include_grounding": True,
                "traffic_class": "benchmark",
            }
            started = time.perf_counter()
            try:
                resp = client.post(
                    f"{args.base_url.rstrip('/')}/memory/search",
                    headers=headers,
                    json=body,
                )
                elapsed_ms = (time.perf_counter() - started) * 1000.0
                timings.append(elapsed_ms)
                if resp.status_code >= 400:
                    errors.append(f"{resp.status_code}:{resp.text[:120]}")
            except Exception as exc:  # pragma: no cover - network/runtime dependent
                elapsed_ms = (time.perf_counter() - started) * 1000.0
                timings.append(elapsed_ms)
                errors.append(str(exc))

    timings_sorted = sorted(timings)
    return {
        "generatedAt": datetime.now(timezone.utc).isoformat(),
        "baseUrl": args.base_url,
        "requests": args.requests,
        "runtime": runtime_payload,
        "latencyMs": {
            "p50": round(percentile(timings_sorted, 0.50), 3),
            "p95": round(percentile(timings_sorted, 0.95), 3),
            "p99": round(percentile(timings_sorted, 0.99), 3),
            "mean": round(statistics.mean(timings_sorted), 3) if timings_sorted else 0.0,
        },
        "errors": errors,
        "errorCount": len(errors),
    }


def main() -> None:
    parser = argparse.ArgumentParser(description="Phase 1+ migration runtime benchmark")
    parser.add_argument("--base-url", default="http://127.0.0.1:8075")
    parser.add_argument("--api-key", default="")
    parser.add_argument("--requests", type=int, default=20)
    parser.add_argument("--timeout", type=float, default=45.0)
    parser.add_argument("--output", default="")
    args = parser.parse_args()

    result = run(args)
    output = json.dumps(result, indent=2, sort_keys=True)
    print(output)

    if args.output:
        target = Path(args.output)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(output + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
