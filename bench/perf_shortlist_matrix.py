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
    vary_query: bool = False,
) -> dict[str, Any]:
    latencies: list[float] = []
    errors: list[str] = []
    for idx in range(max(1, runs)):
        effective_query = query
        if vary_query:
            effective_query = f"{query} :: run-{idx + 1}"
        started = time.perf_counter()
        try:
            resp = client.post(
                f"{base_url}/memory/search",
                headers=headers,
                json={
                    "query": effective_query,
                    "project": project,
                    "limit": limit,
                    "include_retrieval_debug": True,
                    "include_grounding": True,
                    "retrieval_mode": mode,
                    "traffic_class": "benchmark",
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


def _case_error_rate(case_payload: dict[str, Any]) -> float:
    runs = max(1, int(case_payload.get("runs") or 0))
    error_count = max(0, int(case_payload.get("errorCount") or 0))
    return float(error_count) / float(runs)


def _embedding_case_metrics(matrix: dict[str, Any] | None) -> tuple[float, float] | None:
    if not isinstance(matrix, dict):
        return None
    case_payload = matrix.get("embedding_stress")
    if not isinstance(case_payload, dict):
        return None
    p95 = float(((case_payload.get("latencyMs") or {}).get("p95") or 0.0))
    error_rate = _case_error_rate(case_payload)
    if p95 <= 0:
        return None
    return p95, error_rate


def _aggregate(values: list[float], strategy: str) -> float:
    if not values:
        return 0.0
    token = str(strategy or "median").strip().lower()
    if token == "mean":
        return float(statistics.mean(values))
    if token == "min":
        return float(min(values))
    if token == "max":
        return float(max(values))
    return float(statistics.median(values))


def evaluate_fastembed_gate(
    current_matrix: dict[str, Any],
    *,
    baseline_matrix: dict[str, Any] | None,
    min_improvement_pct: float,
    max_error_regression: float,
    sample_matrices: list[dict[str, Any]] | None = None,
    aggregate_strategy: str = "median",
) -> dict[str, Any]:
    metrics_rows: list[tuple[float, float]] = []
    if isinstance(sample_matrices, list) and sample_matrices:
        for matrix_payload in sample_matrices:
            metrics = _embedding_case_metrics(matrix_payload)
            if metrics is not None:
                metrics_rows.append(metrics)
    if not metrics_rows:
        metrics = _embedding_case_metrics(current_matrix)
        if metrics is not None:
            metrics_rows.append(metrics)
    if not metrics_rows:
        return {
            "passed": False,
            "reason": "missing_embedding_stress_case",
        }
    baseline_metrics = _embedding_case_metrics(baseline_matrix)
    if baseline_metrics is None:
        return {
            "passed": False,
            "reason": "baseline_matrix_missing",
        }
    baseline_p95, baseline_error_rate = baseline_metrics
    current_p95_values = [row[0] for row in metrics_rows]
    current_error_values = [row[1] for row in metrics_rows]
    current_p95 = _aggregate(current_p95_values, aggregate_strategy)
    current_error_rate = _aggregate(current_error_values, "mean")
    improvement_pct = ((baseline_p95 - current_p95) / baseline_p95) * 100.0
    error_regressions = [value - baseline_error_rate for value in current_error_values]
    error_regression = (
        max(error_regressions)
        if error_regressions
        else (current_error_rate - baseline_error_rate)
    )
    passed = bool(
        improvement_pct >= float(min_improvement_pct)
        and error_regression <= float(max_error_regression)
    )
    return {
        "passed": passed,
        "reason": "ok" if passed else "threshold_not_met",
        "thresholds": {
            "minImprovementPct": float(min_improvement_pct),
            "maxErrorRegression": float(max_error_regression),
        },
        "metrics": {
            "sampleCount": len(metrics_rows),
            "aggregateStrategy": str(aggregate_strategy or "median"),
            "sampleP95Ms": [round(float(value), 3) for value in current_p95_values],
            "sampleErrorRates": [round(float(value), 6) for value in current_error_values],
            "baselineP95Ms": round(baseline_p95, 3),
            "currentP95Ms": round(current_p95, 3),
            "improvementPct": round(improvement_pct, 3),
            "baselineErrorRate": round(baseline_error_rate, 6),
            "currentErrorRate": round(current_error_rate, 6),
            "errorRegression": round(error_regression, 6),
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
            "vary_query": False,
        },
        "deep_recall": {
            "query": "deep retrieval stability timeout source quality",
            "mode": "deep",
            "limit": 16,
            "vary_query": False,
        },
        "ops_focus": {
            "query": "fanout queue pressure deadletters letta backlog",
            "mode": "balanced",
            "limit": 12,
            "vary_query": False,
        },
        "embedding_stress": {
            "query": "embedding adapter throughput probe",
            "mode": "fast",
            "limit": 10,
            "vary_query": True,
        },
    }
    matrix: dict[str, Any] = {}
    baseline_payload: dict[str, Any] | None = None
    baseline_matrix: dict[str, Any] | None = None
    baseline_path = str(args.baseline or "").strip()
    if baseline_path:
        try:
            baseline_payload = json.loads(Path(baseline_path).read_text(encoding="utf-8"))
            matrix_payload = baseline_payload.get("matrix")
            if isinstance(matrix_payload, dict):
                baseline_matrix = matrix_payload
        except Exception as exc:  # pragma: no cover - filesystem/runtime dependent
            baseline_payload = {"error": str(exc)}
    gate_warmups = max(0, int(args.gate_warmups))
    gate_repeats = max(1, int(args.gate_repeats))
    gate_total_runs = gate_warmups + gate_repeats
    gate_sample_matrices: list[dict[str, Any]] = []
    gate_sample_rows: list[dict[str, Any]] = []
    with httpx.Client(timeout=args.timeout) as client:
        source_quality = None
        adapter_metrics_before = None
        adapter_metrics_after = None
        metrics_before = client.get(f"{base_url}/telemetry/metrics", headers=headers)
        if metrics_before.status_code < 400:
            payload_before = metrics_before.json()
            embedding_cache_before = payload_before.get("embeddingCache") if isinstance(payload_before, dict) else None
            if isinstance(embedding_cache_before, dict):
                adapter_metrics_before = embedding_cache_before.get("fastembedRs")
        for run_index in range(gate_total_runs):
            current_matrix: dict[str, Any] = {}
            with client.stream(
                "GET",
                f"{base_url}/telemetry/retrieval/source-quality?traffic_class=benchmark",
                headers=headers,
            ) as resp:
                if resp.status_code < 400:
                    source_quality = json.loads(resp.read().decode("utf-8"))
            for name, case in cases.items():
                current_matrix[name] = run_case(
                    client,
                    base_url,
                    headers,
                    query=str(case["query"]),
                    project=args.project,
                    mode=str(case["mode"]),
                    limit=int(case["limit"]),
                    runs=args.runs,
                    vary_query=bool(case.get("vary_query", False)),
                )
            matrix = current_matrix
            evaluated_index = run_index - gate_warmups
            if evaluated_index >= 0:
                gate_sample_matrices.append(current_matrix)
                metrics = _embedding_case_metrics(current_matrix)
                if metrics is not None:
                    gate_sample_rows.append(
                        {
                            "run": evaluated_index + 1,
                            "p95Ms": round(float(metrics[0]), 3),
                            "errorRate": round(float(metrics[1]), 6),
                        }
                    )
            if run_index + 1 < gate_total_runs and args.gate_sleep_secs > 0:
                time.sleep(float(args.gate_sleep_secs))
        metrics_after = client.get(f"{base_url}/telemetry/metrics", headers=headers)
        if metrics_after.status_code < 400:
            payload_after = metrics_after.json()
            embedding_cache_after = payload_after.get("embeddingCache") if isinstance(payload_after, dict) else None
            if isinstance(embedding_cache_after, dict):
                adapter_metrics_after = embedding_cache_after.get("fastembedRs")
    gate_evaluation = evaluate_fastembed_gate(
        matrix,
        baseline_matrix=baseline_matrix,
        min_improvement_pct=float(args.gate_min_improvement_pct),
        max_error_regression=float(args.gate_max_error_regression),
        sample_matrices=gate_sample_matrices,
        aggregate_strategy=str(args.gate_aggregate or "median"),
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
        "gateSampling": {
            "warmups": gate_warmups,
            "evaluatedRuns": gate_repeats,
            "aggregateStrategy": str(args.gate_aggregate or "median"),
            "samples": gate_sample_rows,
        },
        "sourceQuality": source_quality,
        "adapterTelemetry": {
            "before": adapter_metrics_before,
            "after": adapter_metrics_after,
        },
        "baseline": baseline_payload if baseline_payload is not None else None,
        "gateEvaluation": gate_evaluation,
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
    parser.add_argument("--baseline", default="")
    parser.add_argument("--gate-min-improvement-pct", type=float, default=20.0)
    parser.add_argument("--gate-max-error-regression", type=float, default=0.005)
    parser.add_argument("--gate-repeats", type=int, default=1)
    parser.add_argument("--gate-warmups", type=int, default=0)
    parser.add_argument(
        "--gate-aggregate",
        default="median",
        choices=["median", "mean", "min", "max"],
    )
    parser.add_argument("--gate-sleep-secs", type=float, default=0.0)
    parser.add_argument("--gate-output", default="")
    parser.add_argument("--output", default="")
    args = parser.parse_args()
    payload = run(args)
    rendered = json.dumps(payload, indent=2, sort_keys=True)
    print(rendered)
    output = args.output or f"bench/results/perf_shortlist_matrix_{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}.json"
    path = Path(output)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(rendered + "\n", encoding="utf-8")
    gate_output = str(args.gate_output or "").strip()
    if gate_output:
        gate_payload = {
            "generatedAt": payload.get("generatedAt"),
            "passed": bool((payload.get("gateEvaluation") or {}).get("passed")),
            "reason": (payload.get("gateEvaluation") or {}).get("reason"),
            "thresholds": (payload.get("gateEvaluation") or {}).get("thresholds"),
            "metrics": (payload.get("gateEvaluation") or {}).get("metrics"),
            "sampling": payload.get("gateSampling"),
            "sourceResult": str(path),
        }
        gate_path = Path(gate_output)
        gate_path.parent.mkdir(parents=True, exist_ok=True)
        gate_path.write_text(json.dumps(gate_payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
