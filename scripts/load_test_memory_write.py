#!/usr/bin/env python3
"""Lightweight load test for /memory/write (stdlib only)."""
from __future__ import annotations

import argparse
import concurrent.futures
import json
import time
import urllib.error
import urllib.request


def _make_payload(project: str, index: int, content_size: int, topic_path: str | None) -> bytes:
    content = "x" * max(1, content_size)
    file_name = f"perf/{int(time.time())}_{index}.md"
    payload = {
        "projectName": project,
        "fileName": file_name,
        "content": content,
    }
    if topic_path:
        payload["topicPath"] = topic_path
    return json.dumps(payload).encode("utf-8")


def _send_request(url: str, payload: bytes, timeout: float, api_key: str | None) -> tuple[bool, float]:
    start = time.perf_counter()
    headers = {"content-type": "application/json"}
    if api_key:
        headers["x-api-key"] = api_key
    req = urllib.request.Request(
        url,
        data=payload,
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            _ = resp.read()
            ok = 200 <= resp.status < 300
    except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError):
        ok = False
    return ok, time.perf_counter() - start


def _percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    values = sorted(values)
    idx = int(round((pct / 100.0) * (len(values) - 1)))
    return values[idx]


def main() -> int:
    parser = argparse.ArgumentParser(description="Load test memMCP /memory/write")
    parser.add_argument("--url", default="http://127.0.0.1:8075/memory/write")
    parser.add_argument("--project", default="perf_test")
    parser.add_argument("--rate", type=float, default=100.0, help="requests per second")
    parser.add_argument("--seconds", type=int, default=10, help="duration in seconds")
    parser.add_argument("--threads", type=int, default=20, help="worker threads")
    parser.add_argument("--payload-bytes", type=int, default=256)
    parser.add_argument("--timeout", type=float, default=5.0)
    parser.add_argument("--topic-path", default=None)
    parser.add_argument("--api-key", default=None, help="Optional x-api-key header value")
    args = parser.parse_args()

    total = int(args.rate * args.seconds)
    if total <= 0:
        print("Nothing to send.")
        return 0

    latencies: list[float] = []
    success = 0
    start_time = time.perf_counter()
    next_time = start_time

    with concurrent.futures.ThreadPoolExecutor(max_workers=args.threads) as executor:
        futures: list[concurrent.futures.Future[tuple[bool, float]]] = []
        for i in range(total):
            now = time.perf_counter()
            if now < next_time:
                time.sleep(next_time - now)
            payload = _make_payload(args.project, i, args.payload_bytes, args.topic_path)
            futures.append(executor.submit(_send_request, args.url, payload, args.timeout, args.api_key))
            next_time += 1.0 / args.rate

        for fut in concurrent.futures.as_completed(futures):
            ok, latency = fut.result()
            latencies.append(latency)
            if ok:
                success += 1

    elapsed = time.perf_counter() - start_time
    rps = total / elapsed if elapsed else 0.0
    p50 = _percentile(latencies, 50)
    p90 = _percentile(latencies, 90)
    p95 = _percentile(latencies, 95)
    p99 = _percentile(latencies, 99)

    print("Load test complete")
    print(f"  total: {total}")
    print(f"  success: {success}")
    print(f"  fail: {total - success}")
    print(f"  elapsed: {elapsed:.2f}s")
    print(f"  achieved_rps: {rps:.2f}")
    print(f"  latency_p50: {p50*1000:.1f}ms")
    print(f"  latency_p90: {p90*1000:.1f}ms")
    print(f"  latency_p95: {p95*1000:.1f}ms")
    print(f"  latency_p99: {p99*1000:.1f}ms")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
