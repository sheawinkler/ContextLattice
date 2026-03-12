#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import statistics
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

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
    return (values[low] * (1.0 - weight)) + (values[high] * weight)


def run_case(
    client: httpx.Client,
    base_url: str,
    headers: dict[str, str],
    *,
    query: str,
    project: str,
    mode: str,
    limit: int,
    runs: int,
) -> dict[str, Any]:
    latencies: list[float] = []
    errors: list[str] = []
    for _ in range(max(1, runs)):
        started = time.perf_counter()
        try:
            resp = client.post(
                f"{base_url}/memory/search",
                headers=headers,
                json={
                    "query": query,
                    "project": project,
                    "limit": limit,
                    "include_retrieval_debug": True,
                    "include_grounding": True,
                    "retrieval_mode": mode,
                },
            )
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            latencies.append(elapsed_ms)
            if resp.status_code >= 400:
                errors.append(f"{resp.status_code}:{resp.text[:120]}")
        except Exception as exc:  # pragma: no cover - runtime/network dependent
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            latencies.append(elapsed_ms)
            errors.append(str(exc))
    latencies.sort()
    return {
        "runs": len(latencies),
        "errors": errors,
        "errorCount": len(errors),
        "latencyMs": {
            "p50": round(percentile(latencies, 0.50), 3),
            "p95": round(percentile(latencies, 0.95), 3),
            "p99": round(percentile(latencies, 0.99), 3),
            "mean": round(statistics.mean(latencies), 3) if latencies else 0.0,
            "min": round(min(latencies), 3) if latencies else 0.0,
            "max": round(max(latencies), 3) if latencies else 0.0,
        },
    }


def run(args: argparse.Namespace) -> dict[str, Any]:
    headers = {"content-type": "application/json"}
    if args.api_key:
        headers["x-api-key"] = args.api_key
    base_url = args.base_url.rstrip("/")
    cases = {
        "short_context": {
            "query": "source quality",
            "mode": "fast",
            "limit": 8,
        },
        "deep_recall": {
            "query": "deep retrieval stability timeout source quality",
            "mode": "deep",
            "limit": 16,
        },
        "ops_focus": {
            "query": "fanout queue pressure deadletters letta backlog",
            "mode": "balanced",
            "limit": 12,
        },
    }
    matrix: dict[str, Any] = {}
    with httpx.Client(timeout=args.timeout) as client:
        source_quality = None
        with client.stream("GET", f"{base_url}/telemetry/retrieval/source-quality", headers=headers) as resp:
            if resp.status_code < 400:
                source_quality = json.loads(resp.read().decode("utf-8"))
        for name, case in cases.items():
            matrix[name] = run_case(
                client,
                base_url,
                headers,
                query=str(case["query"]),
                project=args.project,
                mode=str(case["mode"]),
                limit=int(case["limit"]),
                runs=args.runs,
            )
    return {
        "generatedAt": datetime.now(timezone.utc).isoformat(),
        "baseUrl": base_url,
        "project": args.project,
        "runsPerCase": args.runs,
        "candidates": {
            "fastembed-rs": "adapter spike pending",
            "EmbedAnything": "adapter spike pending",
            "edgevec": "adapter spike pending",
            "zvec": "benchmark-only candidate",
            "swiftide": "pipeline pattern reference",
        },
        "matrix": matrix,
        "sourceQuality": source_quality,
        "notes": [
            "Use this script before/after each adapter spike.",
            "Keep runtime defaults unchanged unless benchmark + recall gates pass.",
        ],
    }


def main() -> None:
    parser = argparse.ArgumentParser(description="Shortlist candidate performance matrix runner")
    parser.add_argument("--base-url", default="http://127.0.0.1:8075")
    parser.add_argument("--api-key", default="")
    parser.add_argument("--project", default="perf_shortlist")
    parser.add_argument("--runs", type=int, default=12)
    parser.add_argument("--timeout", type=float, default=45.0)
    parser.add_argument("--output", default="")
    args = parser.parse_args()
    payload = run(args)
    rendered = json.dumps(payload, indent=2, sort_keys=True)
    print(rendered)
    output = args.output or f"bench/results/perf_shortlist_matrix_{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}.json"
    path = Path(output)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(rendered + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
