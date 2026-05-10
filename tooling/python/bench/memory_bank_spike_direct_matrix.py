#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import math
import statistics
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import httpx


BACKENDS = (
    "quickwit_spike",
    "meilisearch_spike",
    "tantivy_spike",
    "lancedb_spike",
    "trieve_spike",
    "helixdb_spike",
    "icm_spike",
    "shodh_spike",
    "memvid_spike",
    "surrealdb_spike",
)
DEFAULT_BACKENDS = ("quickwit_spike", "meilisearch_spike", "tantivy_spike")


@dataclass(frozen=True)
class CaseConfig:
    query: str
    limit: int


CASES: dict[str, CaseConfig] = {
    "short_context": CaseConfig(
        query="source quality qdrant topic rollups",
        limit=8,
    ),
    "deep_read": CaseConfig(
        query="deep retrieval stability timeout source quality",
        limit=16,
    ),
    "ops_focus": CaseConfig(
        query="fanout queue pressure deadletters backlog quality",
        limit=12,
    ),
}


def percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    if len(values) == 1:
        return float(values[0])
    idx = (len(values) - 1) * pct
    low = int(math.floor(idx))
    high = min(len(values) - 1, low + 1)
    weight = idx - low
    return (values[low] * (1.0 - weight)) + (values[high] * weight)


def _compact_float(value: float) -> float:
    return round(float(value), 3)


def run_case(
    client: httpx.Client,
    *,
    base_url: str,
    backend: str,
    case_name: str,
    case: CaseConfig,
    project: str,
    runs: int,
    warmups: int,
    cache_bust: bool,
) -> dict[str, Any]:
    latencies: list[float] = []
    status_codes: list[int] = []
    errors: list[str] = []
    warmup_errors: list[str] = []
    result_counts: list[int] = []
    top_scores: list[float] = []

    for idx in range(max(1, runs) + max(0, warmups)):
        is_warmup = idx < max(0, warmups)
        if is_warmup:
            run_number = idx + 1
        else:
            run_number = idx + 1 - max(0, warmups)
        query = case.query
        if cache_bust:
            tag = "warmup" if is_warmup else "run"
            query = f"{query} {tag}_{run_number}"
        payload = {
            "query": query,
            "limit": case.limit,
            "project": project,
            "backend": backend,
        }
        started = time.perf_counter()
        try:
            resp = client.post(f"{base_url}/search", json=payload)
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            if is_warmup:
                if resp.status_code >= 400:
                    warmup_errors.append(f"{resp.status_code}:{resp.text[:200]}")
                continue
            latencies.append(elapsed_ms)
            status_codes.append(int(resp.status_code))
            if resp.status_code >= 400:
                errors.append(f"{resp.status_code}:{resp.text[:200]}")
                result_counts.append(0)
                top_scores.append(0.0)
                continue
            body = resp.json()
            rows = body.get("results") if isinstance(body, dict) else []
            if not isinstance(rows, list):
                rows = []
            result_counts.append(len(rows))
            top = rows[0] if rows else {}
            top_scores.append(float(top.get("score") or 0.0) if isinstance(top, dict) else 0.0)
        except Exception as exc:  # noqa: BLE001
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            if is_warmup:
                warmup_errors.append(str(exc))
                continue
            latencies.append(elapsed_ms)
            status_codes.append(0)
            errors.append(str(exc))
            result_counts.append(0)
            top_scores.append(0.0)

    latencies.sort()
    status_failures = sum(1 for code in status_codes if code >= 400 or code == 0)
    error_rate = float(status_failures) / float(max(1, len(status_codes)))
    return {
        "case": case_name,
        "warmups": int(max(0, warmups)),
        "warmupErrorCount": len(warmup_errors),
        "warmupErrors": warmup_errors,
        "runs": len(status_codes),
        "statusCodes": status_codes,
        "errorCount": len(errors),
        "errors": errors,
        "errorRate": round(error_rate, 6),
        "latencyMs": {
            "p50": _compact_float(percentile(latencies, 0.50)),
            "p95": _compact_float(percentile(latencies, 0.95)),
            "p99": _compact_float(percentile(latencies, 0.99)),
            "mean": _compact_float(statistics.mean(latencies) if latencies else 0.0),
            "min": _compact_float(min(latencies) if latencies else 0.0),
            "max": _compact_float(max(latencies) if latencies else 0.0),
        },
        "resultCount": {
            "mean": _compact_float(statistics.mean(result_counts) if result_counts else 0.0),
            "min": int(min(result_counts) if result_counts else 0),
            "max": int(max(result_counts) if result_counts else 0),
        },
        "topScore": {
            "mean": _compact_float(statistics.mean(top_scores) if top_scores else 0.0),
            "max": _compact_float(max(top_scores) if top_scores else 0.0),
        },
    }


def summarize_backend(case_rows: dict[str, dict[str, Any]]) -> dict[str, Any]:
    p95_values: list[float] = []
    error_rates: list[float] = []
    mean_results: list[float] = []
    for row in case_rows.values():
        latency = row.get("latencyMs") if isinstance(row.get("latencyMs"), dict) else {}
        p95_values.append(float(latency.get("p95") or 0.0))
        error_rates.append(float(row.get("errorRate") or 0.0))
        mean_results.append(float(row.get("resultCount", {}).get("mean") or 0.0))
    return {
        "avgP95Ms": _compact_float(statistics.mean(p95_values) if p95_values else 0.0),
        "maxP95Ms": _compact_float(max(p95_values) if p95_values else 0.0),
        "avgErrorRate": round(statistics.mean(error_rates) if error_rates else 0.0, 6),
        "avgResultCount": _compact_float(statistics.mean(mean_results) if mean_results else 0.0),
    }


def run(args: argparse.Namespace) -> dict[str, Any]:
    base_url = str(args.base_url).rstrip("/")
    selected_backends = [token.strip() for token in str(args.backends).split(",") if token.strip()]
    if not selected_backends:
        selected_backends = list(BACKENDS)

    selected_cases = [token.strip() for token in str(args.cases).split(",") if token.strip()]
    if not selected_cases:
        selected_cases = list(CASES.keys())

    unknown_backends = [name for name in selected_backends if name not in BACKENDS]
    if unknown_backends:
        raise SystemExit(f"Unknown backend(s): {', '.join(unknown_backends)}")

    unknown_cases = [name for name in selected_cases if name not in CASES]
    if unknown_cases:
        raise SystemExit(f"Unknown case(s): {', '.join(unknown_cases)}")

    with httpx.Client(timeout=float(args.timeout)) as client:
        health = {}
        try:
            resp = client.get(f"{base_url}/health")
            if resp.status_code < 400:
                health = resp.json() if isinstance(resp.json(), dict) else {}
        except Exception:
            health = {}

        results: dict[str, Any] = {}
        for backend in selected_backends:
            case_rows: dict[str, Any] = {}
            for case_name in selected_cases:
                case_rows[case_name] = run_case(
                    client,
                    base_url=base_url,
                    backend=backend,
                    case_name=case_name,
                    case=CASES[case_name],
                    project=args.project,
                    runs=max(1, int(args.runs)),
                    warmups=max(0, int(args.warmups)),
                    cache_bust=bool(args.cache_bust),
                )
            results[backend] = {
                "cases": case_rows,
                "summary": summarize_backend(case_rows),
            }

    recommendations: list[str] = []
    for backend in selected_backends:
        backend_summary = results.get(backend, {}).get("summary", {})
        avg_results = float(backend_summary.get("avgResultCount") or 0.0)
        if avg_results <= 0.0:
            recommendations.append(
                f"{backend}: zero direct hits; verify ingest sync and analyzer compatibility before promotion."
            )

    return {
        "generatedAt": datetime.now(timezone.utc).isoformat(),
        "baseUrl": base_url,
        "project": args.project,
        "runsPerCase": int(args.runs),
        "cases": selected_cases,
        "backends": selected_backends,
        "cacheBustQueries": bool(args.cache_bust),
        "health": health,
        "results": results,
        "recommendations": recommendations,
    }


def main() -> None:
    parser = argparse.ArgumentParser(
        description=(
            "Direct Rust memory-bank spike backend matrix "
            "(meili/quickwit/tantivy + optional adapter/external lanes)."
        )
    )
    parser.add_argument("--base-url", default="http://127.0.0.1:8096")
    parser.add_argument("--project", default="contextlattice")
    parser.add_argument("--runs", type=int, default=5)
    parser.add_argument("--warmups", type=int, default=1)
    parser.add_argument("--timeout", type=float, default=45.0)
    parser.add_argument("--backends", default=",".join(DEFAULT_BACKENDS))
    parser.add_argument("--cases", default=",".join(CASES.keys()))
    parser.add_argument("--cache-bust", action="store_true", default=True)
    parser.add_argument("--no-cache-bust", dest="cache_bust", action="store_false")
    parser.add_argument("--output", default="")
    args = parser.parse_args()

    payload = run(args)
    rendered = json.dumps(payload, indent=2, sort_keys=True)
    print(rendered)

    output = args.output or (
        "bench/results/memory_bank_spike_direct_matrix_"
        + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        + ".json"
    )
    output_path = Path(output)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    output_path.write_text(rendered + "\n", encoding="utf-8")
    latest = output_path.parent / "memory_bank_spike_direct_matrix_latest.json"
    latest.write_text(rendered + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
