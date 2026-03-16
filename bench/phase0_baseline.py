#!/usr/bin/env python3
from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import statistics
import subprocess
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable
from urllib import error, request


def _iso_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _load_api_key(explicit: str | None) -> str:
    if explicit:
        return explicit.strip()
    for name in ("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "MEMMCP_ORCHESTRATOR_API_KEY"):
        value = os.getenv(name, "").strip()
        if value:
            return value
    return ""


def _percentile(values_ms: list[float], pct: float) -> float:
    if not values_ms:
        return 0.0
    ordered = sorted(float(v) for v in values_ms)
    if len(ordered) == 1:
        return ordered[0]
    rank = (len(ordered) - 1) * max(0.0, min(1.0, pct))
    low = int(rank)
    high = min(low + 1, len(ordered) - 1)
    if low == high:
        return ordered[low]
    weight = rank - low
    return ordered[low] + ((ordered[high] - ordered[low]) * weight)


@dataclass
class RequestResult:
    ok: bool
    status: int
    latency_ms: float
    result_count: int
    error: str | None


class BenchmarkClient:
    def __init__(self, base_url: str, api_key: str, timeout_secs: float):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout_secs = max(1.0, float(timeout_secs))

    def call(self, method: str, path: str, payload: dict[str, Any] | None = None) -> RequestResult:
        body = json.dumps(payload).encode("utf-8") if payload is not None else None
        headers = {"content-type": "application/json"}
        if self.api_key:
            headers["x-api-key"] = self.api_key
        req = request.Request(f"{self.base_url}{path}", data=body, headers=headers, method=method)

        started = time.perf_counter()
        try:
            with request.urlopen(req, timeout=self.timeout_secs) as resp:
                raw = resp.read().decode("utf-8")
                elapsed_ms = (time.perf_counter() - started) * 1000
                parsed = json.loads(raw) if raw else {}
                if not isinstance(parsed, dict):
                    parsed = {"raw": parsed}
                result_count = 0
                if isinstance(parsed.get("results"), list):
                    result_count = len(parsed["results"])
                elif isinstance(parsed.get("context_pack"), dict):
                    facts = parsed["context_pack"].get("factual_context")
                    if isinstance(facts, list):
                        result_count = len(facts)
                return RequestResult(
                    ok=200 <= int(resp.status) < 300,
                    status=int(resp.status),
                    latency_ms=elapsed_ms,
                    result_count=result_count,
                    error=None,
                )
        except error.HTTPError as exc:
            elapsed_ms = (time.perf_counter() - started) * 1000
            text = exc.read().decode("utf-8", errors="replace")
            return RequestResult(
                ok=False,
                status=int(exc.code),
                latency_ms=elapsed_ms,
                result_count=0,
                error=text[:280],
            )
        except Exception as exc:  # pragma: no cover
            elapsed_ms = (time.perf_counter() - started) * 1000
            return RequestResult(
                ok=False,
                status=-1,
                latency_ms=elapsed_ms,
                result_count=0,
                error=str(exc)[:280],
            )


PayloadFactory = Callable[[int], tuple[str, str, dict[str, Any] | None]]


def _run_workload(
    *,
    name: str,
    count: int,
    concurrency: int,
    factory: PayloadFactory,
    client: BenchmarkClient,
) -> dict[str, Any]:
    count = max(1, int(count))
    concurrency = max(1, int(concurrency))
    latencies: list[float] = []
    statuses: dict[str, int] = {}
    errors: list[str] = []
    result_counts: list[int] = []

    started = time.perf_counter()

    def _call(index: int) -> RequestResult:
        method, path, payload = factory(index)
        return client.call(method, path, payload)

    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
        futures = [pool.submit(_call, idx) for idx in range(count)]
        for fut in concurrent.futures.as_completed(futures):
            res = fut.result()
            latencies.append(res.latency_ms)
            statuses[str(res.status)] = int(statuses.get(str(res.status), 0) or 0) + 1
            result_counts.append(res.result_count)
            if not res.ok and res.error:
                errors.append(res.error)

    duration_s = max(0.001, time.perf_counter() - started)
    success = sum(v for k, v in statuses.items() if k.isdigit() and 200 <= int(k) < 300)
    fail = count - success
    return {
        "name": name,
        "requests": count,
        "concurrency": concurrency,
        "duration_secs": round(duration_s, 4),
        "throughput_rps": round(count / duration_s, 4),
        "success": success,
        "fail": fail,
        "status_counts": statuses,
        "result_count_avg": round(statistics.mean(result_counts), 4) if result_counts else 0.0,
        "latency_ms": {
            "min": round(min(latencies), 3) if latencies else 0.0,
            "avg": round(statistics.mean(latencies), 3) if latencies else 0.0,
            "p50": round(_percentile(latencies, 0.50), 3),
            "p95": round(_percentile(latencies, 0.95), 3),
            "p99": round(_percentile(latencies, 0.99), 3),
            "max": round(max(latencies), 3) if latencies else 0.0,
        },
        "sample_errors": errors[:5],
    }


def _docker_stats_snapshot() -> dict[str, Any]:
    try:
        proc = subprocess.run(
            ["docker", "stats", "--no-stream", "--format", "{{json .}}"],
            check=True,
            capture_output=True,
            text=True,
            timeout=15,
        )
    except Exception as exc:
        return {"error": str(exc)}

    rows: list[dict[str, Any]] = []
    for line in (proc.stdout or "").splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            parsed = json.loads(line)
        except Exception:
            continue
        if not isinstance(parsed, dict):
            continue
        name = str(parsed.get("Name") or parsed.get("Container") or "")
        if not name:
            continue
        rows.append(
            {
                "name": name,
                "cpu": parsed.get("CPUPerc"),
                "mem": parsed.get("MemUsage"),
                "mem_perc": parsed.get("MemPerc"),
                "net_io": parsed.get("NetIO"),
                "block_io": parsed.get("BlockIO"),
            }
        )
    rows.sort(key=lambda item: str(item.get("name") or ""))
    return {"containers": rows}


def _http_json(client: BenchmarkClient, path: str) -> dict[str, Any]:
    res = client.call("GET", path, None)
    payload: dict[str, Any] = {
        "ok": res.ok,
        "status": res.status,
        "latency_ms": round(res.latency_ms, 3),
    }
    if res.error:
        payload["error"] = res.error
    return payload


def _build_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Run ContextLattice Phase 0 performance baseline workloads.")
    parser.add_argument("--base-url", default="http://127.0.0.1:8075")
    parser.add_argument("--api-key", default=None)
    parser.add_argument("--project", default="perf_phase0")
    parser.add_argument("--timeout-secs", type=float, default=75.0)

    parser.add_argument("--single-requests", type=int, default=30)
    parser.add_argument("--multi-requests", type=int, default=24)
    parser.add_argument("--retrieval-requests", type=int, default=6)
    parser.add_argument("--write-requests", type=int, default=60)

    parser.add_argument("--single-concurrency", type=int, default=1)
    parser.add_argument("--multi-concurrency", type=int, default=6)
    parser.add_argument("--retrieval-concurrency", type=int, default=2)
    parser.add_argument("--write-concurrency", type=int, default=12)

    parser.add_argument(
        "--output",
        default=None,
        help="Output JSON path (default: bench/results/phase0_baseline_<timestamp>.json)",
    )
    return parser.parse_args()


def main() -> int:
    args = _build_args()
    api_key = _load_api_key(args.api_key)
    if not api_key:
        print("ERROR: missing API key (set CONTEXTLATTICE_ORCHESTRATOR_API_KEY or MEMMCP_ORCHESTRATOR_API_KEY).")
        return 2

    run_id = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    output = Path(args.output or f"bench/results/phase0_baseline_{run_id}.json")
    output.parent.mkdir(parents=True, exist_ok=True)

    client = BenchmarkClient(args.base_url, api_key, args.timeout_secs)

    status_before = _http_json(client, "/status")
    retrieval_before = _http_json(client, "/telemetry/retrieval")
    recall_before = _http_json(client, "/telemetry/recall")
    docker_before = _docker_stats_snapshot()

    short_queries = [
        "source quality",
        "context retrieval",
        "agent memory",
        "orchestrator status",
        "retrieval latency",
    ]
    medium_queries = [
        "summarize current retrieval quality posture with risks and recent updates",
        "what changed in contextlattice retrieval behavior over recent sessions",
        "identify known failure patterns in memory retrieval and mitigation steps",
        "provide a grounded context pack for source quality and orchestration",
    ]
    heavy_queries = [
        "source quality letta mindsdb memory bank qdrant retrieval performance deep analysis",
        "recall degradation timeout causes multi-source retrieval bottlenecks",
        "context lattice memory search deep retrieval diagnostics and source quality",
    ]

    write_project = f"{args.project}_{run_id.lower()}"

    workloads: list[dict[str, Any]] = []
    workloads.append(
        _run_workload(
            name="single_agent_short_context",
            count=args.single_requests,
            concurrency=args.single_concurrency,
            client=client,
            factory=lambda i: (
                "POST",
                "/memory/search",
                {
                    "query": short_queries[i % len(short_queries)],
                    "limit": 5,
                    "retrieval_mode": "fast",
                    "agent_id": "phase0-single",
                    "include_grounding": True,
                    "query_expansion": False,
                    "traffic_class": "benchmark",
                },
            ),
        )
    )

    workloads.append(
        _run_workload(
            name="multi_agent_medium_context",
            count=args.multi_requests,
            concurrency=args.multi_concurrency,
            client=client,
            factory=lambda i: (
                "POST",
                "/memory/context-pack",
                {
                    "query": medium_queries[i % len(medium_queries)],
                    "project": "_global",
                    "limit": 8,
                    "max_facts": 20,
                    "retrieval_mode": "balanced",
                    "agent_id": f"phase0-multi-{i % 8}",
                    "query_expansion": True,
                    "auto_escalate": True,
                    "traffic_class": "benchmark",
                },
            ),
        )
    )

    workloads.append(
        _run_workload(
            name="retrieval_heavy_queries",
            count=args.retrieval_requests,
            concurrency=args.retrieval_concurrency,
            client=client,
            factory=lambda i: (
                "POST",
                "/memory/search",
                {
                    "query": heavy_queries[i % len(heavy_queries)],
                    "project": "_global",
                    "limit": 10,
                    "retrieval_mode": "deep",
                    "include_retrieval_debug": True,
                    "include_grounding": True,
                    "sources": ["qdrant", "mongo_raw", "mindsdb", "topic_rollups", "letta", "memory_bank"],
                    "agent_id": "phase0-heavy",
                    "query_expansion": True,
                    "auto_escalate": True,
                    "traffic_class": "benchmark",
                },
            ),
        )
    )

    workloads.append(
        _run_workload(
            name="high_frequency_state_updates",
            count=args.write_requests,
            concurrency=args.write_concurrency,
            client=client,
            factory=lambda i: (
                "POST",
                "/memory/write",
                {
                    "projectName": write_project,
                    "fileName": f"bench/phase0_{run_id.lower()}_{i:05d}.txt",
                    "content": (
                        "phase0 baseline write sample "
                        f"run={run_id} idx={i} ts={_iso_now()} query=performance"
                    ),
                    "topicPath": "bench/phase0",
                },
            ),
        )
    )

    status_after = _http_json(client, "/status")
    retrieval_after = _http_json(client, "/telemetry/retrieval")
    recall_after = _http_json(client, "/telemetry/recall")
    docker_after = _docker_stats_snapshot()

    totals = {
        "requests": int(sum(item.get("requests", 0) for item in workloads)),
        "success": int(sum(item.get("success", 0) for item in workloads)),
        "fail": int(sum(item.get("fail", 0) for item in workloads)),
    }
    totals["success_rate"] = round(
        (totals["success"] / totals["requests"]) if totals["requests"] > 0 else 0.0,
        6,
    )

    payload = {
        "run_id": run_id,
        "started_at": _iso_now(),
        "base_url": args.base_url,
        "write_project": write_project,
        "workloads": workloads,
        "totals": totals,
        "snapshots": {
            "status_before": status_before,
            "retrieval_before": retrieval_before,
            "recall_before": recall_before,
            "docker_before": docker_before,
            "status_after": status_after,
            "retrieval_after": retrieval_after,
            "recall_after": recall_after,
            "docker_after": docker_after,
        },
    }

    with output.open("w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2)

    print(f"wrote baseline to {output}")
    for item in workloads:
        print(
            f"{item['name']}: req={item['requests']} ok={item['success']} fail={item['fail']} "
            f"p50={item['latency_ms']['p50']}ms p95={item['latency_ms']['p95']}ms "
            f"rps={item['throughput_rps']}"
        )
    print(json.dumps({"totals": totals}, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
