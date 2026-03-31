#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import math
from collections import defaultdict
from pathlib import Path
from statistics import mean
from typing import Any


def percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    if len(values) == 1:
        return float(values[0])
    sorted_vals = sorted(values)
    rank = p * (len(sorted_vals) - 1)
    low = math.floor(rank)
    high = math.ceil(rank)
    if low == high:
        return float(sorted_vals[low])
    weight = rank - low
    return float(sorted_vals[low] * (1.0 - weight) + sorted_vals[high] * weight)


def load_ndjson(path: Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with path.open("r", encoding="utf-8") as handle:
        for line in handle:
            raw = line.strip()
            if not raw:
                continue
            try:
                payload = json.loads(raw)
            except Exception:
                continue
            if isinstance(payload, dict):
                rows.append(payload)
    return rows


def summarize(rows: list[dict[str, Any]]) -> dict[str, Any]:
    by_mode: dict[str, list[dict[str, Any]]] = defaultdict(list)
    degraded_samples: list[float] = []
    docker_samples: dict[str, list[float]] = defaultdict(list)
    for row in rows:
        degraded_samples.append(float(row.get("degradedRate") or 0.0))
        mode_rows = row.get("modeResults") or []
        if isinstance(mode_rows, list):
            for mode_row in mode_rows:
                if not isinstance(mode_row, dict):
                    continue
                mode = str(mode_row.get("mode") or "unknown").strip().lower() or "unknown"
                by_mode[mode].append(mode_row)
        docker = row.get("dockerMemory") or {}
        if isinstance(docker, dict):
            for service, info in docker.items():
                if not isinstance(info, dict):
                    continue
                used = info.get("usedMiB")
                if isinstance(used, (float, int)):
                    docker_samples[service].append(float(used))

    mode_summary: dict[str, Any] = {}
    for mode, items in by_mode.items():
        latencies = [float(item.get("elapsedMs") or 0.0) for item in items]
        degraded_flags = [1.0 if bool(item.get("degraded")) else 0.0 for item in items]
        http_ok = [1.0 if int(item.get("status") or 0) == 200 else 0.0 for item in items]
        mode_summary[mode] = {
            "requests": len(items),
            "latencyMs": {
                "avg": round(mean(latencies), 3) if latencies else 0.0,
                "p95": round(percentile(latencies, 0.95), 3) if latencies else 0.0,
                "p99": round(percentile(latencies, 0.99), 3) if latencies else 0.0,
                "max": round(max(latencies), 3) if latencies else 0.0,
            },
            "degradedRate": round(mean(degraded_flags), 6) if degraded_flags else 0.0,
            "http200Rate": round(mean(http_ok), 6) if http_ok else 0.0,
        }

    docker_summary: dict[str, Any] = {}
    for service, samples in docker_samples.items():
        if not samples:
            continue
        docker_summary[service] = {
            "samples": len(samples),
            "avgUsedMiB": round(mean(samples), 3),
            "p95UsedMiB": round(percentile(samples, 0.95), 3),
            "maxUsedMiB": round(max(samples), 3),
        }

    return {
        "cycles": len(rows),
        "overallDegradedRate": round(mean(degraded_samples), 6) if degraded_samples else 0.0,
        "modes": mode_summary,
        "dockerMemory": docker_summary,
    }


def compare(current: dict[str, Any], baseline: dict[str, Any]) -> dict[str, Any]:
    out: dict[str, Any] = {
        "overallDegradedRateDelta": round(
            float(current.get("overallDegradedRate") or 0.0) - float(baseline.get("overallDegradedRate") or 0.0),
            6,
        ),
        "modes": {},
    }
    current_modes = current.get("modes") or {}
    baseline_modes = baseline.get("modes") or {}
    all_modes = sorted(set(current_modes.keys()) | set(baseline_modes.keys()))
    for mode in all_modes:
        cur = current_modes.get(mode) or {}
        base = baseline_modes.get(mode) or {}
        cur_lat = (cur.get("latencyMs") or {}) if isinstance(cur, dict) else {}
        base_lat = (base.get("latencyMs") or {}) if isinstance(base, dict) else {}
        out["modes"][mode] = {
            "p95DeltaMs": round(float(cur_lat.get("p95") or 0.0) - float(base_lat.get("p95") or 0.0), 3),
            "p99DeltaMs": round(float(cur_lat.get("p99") or 0.0) - float(base_lat.get("p99") or 0.0), 3),
            "degradedRateDelta": round(
                float(cur.get("degradedRate") or 0.0) - float(base.get("degradedRate") or 0.0),
                6,
            ),
        }
    return out


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Summarize retrieval soak NDJSON logs.")
    parser.add_argument("--input", required=True, help="Path to soak NDJSON log.")
    parser.add_argument("--baseline", default=None, help="Optional baseline soak NDJSON log for deltas.")
    parser.add_argument("--skip-cycles", type=int, default=0, help="Number of initial cycles to exclude as warmup.")
    parser.add_argument(
        "--baseline-skip-cycles",
        type=int,
        default=None,
        help="Warmup cycles to exclude in baseline (defaults to --skip-cycles).",
    )
    parser.add_argument("--output", default=None, help="Optional output JSON path.")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    input_path = Path(args.input)
    rows = load_ndjson(input_path)
    if args.skip_cycles and args.skip_cycles > 0:
        rows = rows[int(args.skip_cycles) :]
    summary = summarize(rows)
    summary["input"] = str(input_path)
    summary["skipCycles"] = int(args.skip_cycles)
    if args.baseline:
        baseline_path = Path(args.baseline)
        baseline_rows = load_ndjson(baseline_path)
        baseline_skip = args.baseline_skip_cycles
        if baseline_skip is None:
            baseline_skip = int(args.skip_cycles)
        if baseline_skip and baseline_skip > 0:
            baseline_rows = baseline_rows[int(baseline_skip) :]
        baseline_summary = summarize(baseline_rows)
        summary["baseline"] = {
            "input": str(baseline_path),
            "skipCycles": int(baseline_skip or 0),
            "summary": baseline_summary,
            "delta": compare(summary, baseline_summary),
        }
    rendered = json.dumps(summary, indent=2, ensure_ascii=True)
    if args.output:
        output_path = Path(args.output)
        output_path.parent.mkdir(parents=True, exist_ok=True)
        output_path.write_text(rendered + "\n", encoding="utf-8")
    print(rendered)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
