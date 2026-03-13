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


def run_profile(
    *,
    client: httpx.Client,
    base_url: str,
    headers: dict[str, str],
    project: str,
    query: str,
    runs: int,
    mode: str,
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
                    "retrieval_mode": mode,
                    "sources": ["qdrant", "topic_rollups"],
                    "source_weights": {"qdrant": 1.0, "topic_rollups": 0.85},
                    "include_retrieval_debug": True,
                    "include_grounding": True,
                    "limit": 12,
                },
            )
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            latencies.append(elapsed_ms)
            if resp.status_code >= 400:
                errors.append(f"{resp.status_code}:{resp.text[:120]}")
        except Exception as exc:  # pragma: no cover - runtime/network dependent
            latencies.append((time.perf_counter() - started) * 1000.0)
            errors.append(str(exc))
    latencies.sort()
    return {
        "runs": len(latencies),
        "errorCount": len(errors),
        "errors": errors,
        "latencyMs": {
            "p50": round(percentile(latencies, 0.50), 3),
            "p95": round(percentile(latencies, 0.95), 3),
            "p99": round(percentile(latencies, 0.99), 3),
            "mean": round(statistics.mean(latencies), 3) if latencies else 0.0,
        },
    }


def run(args: argparse.Namespace) -> dict[str, Any]:
    headers = {"content-type": "application/json"}
    if args.api_key:
        headers["x-api-key"] = args.api_key
    base_url = args.base_url.rstrip("/")

    profiles = {
        "baseline": {
            "mode": "balanced",
            "query": "source quality qdrant tuning baseline",
        },
        "deep_tail": {
            "mode": "deep",
            "query": "deep retrieval qdrant tail latency stress",
        },
        "fast_path": {
            "mode": "fast",
            "query": "fast retrieval qdrant topic rollups",
        },
    }
    results: dict[str, Any] = {}
    with httpx.Client(timeout=args.timeout) as client:
        for name, profile in profiles.items():
            results[name] = run_profile(
                client=client,
                base_url=base_url,
                headers=headers,
                project=args.project,
                query=str(profile["query"]),
                runs=args.runs,
                mode=str(profile["mode"]),
            )
        tuning_snapshot: dict[str, Any] = {}
        try:
            telemetry_resp = client.get(
                f"{base_url}/telemetry/retrieval",
                headers=headers,
            )
            if telemetry_resp.status_code == 200:
                telemetry_payload = telemetry_resp.json()
                if isinstance(telemetry_payload, dict):
                    raw_tuning = telemetry_payload.get("qdrantTuning")
                    if isinstance(raw_tuning, dict):
                        tuning_snapshot = raw_tuning
        except Exception:  # pragma: no cover - network/runtime dependent
            tuning_snapshot = {}
    return {
        "generatedAt": datetime.now(timezone.utc).isoformat(),
        "baseUrl": base_url,
        "project": args.project,
        "runsPerProfile": args.runs,
        "profiles": results,
        "qdrantTuning": tuning_snapshot,
        "notes": [
            "Run once per tuning profile and compare p95/p99 against baseline.",
            "Do not promote a profile with recall-quality regression.",
        ],
    }


def main() -> None:
    parser = argparse.ArgumentParser(description="Qdrant tuning benchmark matrix")
    parser.add_argument("--base-url", default="http://127.0.0.1:8075")
    parser.add_argument("--api-key", default="")
    parser.add_argument("--project", default="perf_qdrant")
    parser.add_argument("--runs", type=int, default=20)
    parser.add_argument("--timeout", type=float, default=45.0)
    parser.add_argument("--output", default="")
    args = parser.parse_args()
    payload = run(args)
    rendered = json.dumps(payload, indent=2, sort_keys=True)
    print(rendered)
    output = args.output or f"bench/results/qdrant_tuning_{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}.json"
    path = Path(output)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(rendered + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
