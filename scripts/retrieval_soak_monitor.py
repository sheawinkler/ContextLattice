#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from urllib import error, request


def _iso_now() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _load_api_key(explicit: str | None) -> str:
    if explicit:
        return explicit
    for name in ("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "CONTEXTLATTICE_ORCHESTRATOR_API_KEY"):
        value = os.getenv(name, "").strip()
        if value:
            return value
    return ""


def _http_json(
    *,
    method: str,
    url: str,
    payload: dict[str, Any] | None,
    api_key: str,
    timeout_secs: float,
) -> tuple[int, dict[str, Any], float]:
    body = json.dumps(payload).encode("utf-8") if payload is not None else None
    headers = {"Content-Type": "application/json"}
    if api_key:
        headers["x-api-key"] = api_key
    req = request.Request(url=url, data=body, headers=headers, method=method)
    started = time.perf_counter()
    try:
        with request.urlopen(req, timeout=timeout_secs) as resp:
            raw = resp.read().decode("utf-8")
            elapsed_ms = (time.perf_counter() - started) * 1000
            parsed = json.loads(raw) if raw else {}
            if not isinstance(parsed, dict):
                parsed = {"raw": parsed}
            return int(resp.status), parsed, elapsed_ms
    except error.HTTPError as exc:
        elapsed_ms = (time.perf_counter() - started) * 1000
        text = exc.read().decode("utf-8")
        try:
            parsed = json.loads(text) if text else {}
        except Exception:
            parsed = {"raw": text[:500]}
        if not isinstance(parsed, dict):
            parsed = {"raw": parsed}
        return int(exc.code), parsed, elapsed_ms
    except Exception as exc:
        elapsed_ms = (time.perf_counter() - started) * 1000
        return -1, {"error": str(exc)}, elapsed_ms


def _parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Run retrieval mode soak checks and telemetry sampling.")
    parser.add_argument("--base-url", default="http://127.0.0.1:8075", help="Orchestrator base URL.")
    parser.add_argument("--api-key", default=None, help="API key (falls back to env vars).")
    parser.add_argument("--query", default="source quality", help="Query text used for soak retrieval calls.")
    parser.add_argument("--project", default=None, help="Optional project filter.")
    parser.add_argument("--limit", type=int, default=5, help="Search result limit.")
    parser.add_argument(
        "--modes",
        default="fast,balanced,deep",
        help="Comma-separated retrieval modes to exercise each cycle.",
    )
    parser.add_argument("--interval-secs", type=float, default=60.0, help="Delay between soak cycles.")
    parser.add_argument("--duration-hours", type=float, default=24.0, help="Total runtime in hours.")
    parser.add_argument("--timeout-secs", type=float, default=60.0, help="HTTP timeout per request.")
    parser.add_argument(
        "--output",
        default=None,
        help="NDJSON output path (default: reports/retrieval_soak_<timestamp>.ndjson)",
    )
    return parser.parse_args()


def main() -> int:
    args = _parse_args()
    api_key = _load_api_key(args.api_key)
    if not api_key:
        print("ERROR: missing API key (set CONTEXTLATTICE_ORCHESTRATOR_API_KEY or CONTEXTLATTICE_ORCHESTRATOR_API_KEY).")
        return 2

    modes = [part.strip().lower() for part in str(args.modes or "").split(",") if part.strip()]
    if not modes:
        modes = ["balanced"]
    base_url = args.base_url.rstrip("/")
    started_iso = _iso_now()
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    output_path = Path(args.output or f"reports/retrieval_soak_{timestamp}.ndjson")
    output_path.parent.mkdir(parents=True, exist_ok=True)
    duration_secs = max(60.0, float(args.duration_hours) * 3600.0)
    deadline = time.monotonic() + duration_secs
    cycle = 0
    print(f"starting soak: output={output_path} duration_hours={args.duration_hours} interval_secs={args.interval_secs}")
    with output_path.open("a", encoding="utf-8") as handle:
        while time.monotonic() < deadline:
            cycle += 1
            cycle_started = time.monotonic()
            mode_results: list[dict[str, Any]] = []
            for mode in modes:
                status, payload, elapsed_ms = _http_json(
                    method="POST",
                    url=f"{base_url}/memory/search",
                    payload={
                        "query": args.query,
                        "limit": max(1, int(args.limit)),
                        "project": args.project,
                        "retrieval_mode": mode,
                        "include_retrieval_debug": True,
                        "traffic_class": "benchmark",
                    },
                    api_key=api_key,
                    timeout_secs=max(5.0, float(args.timeout_secs)),
                )
                retrieval = payload.get("retrieval") if isinstance(payload, dict) else {}
                staged = retrieval.get("staged_fetch") if isinstance(retrieval, dict) else {}
                mode_results.append(
                    {
                        "mode": mode,
                        "status": status,
                        "elapsedMs": round(elapsed_ms, 3),
                        "results": len(payload.get("results") or []) if isinstance(payload, dict) else 0,
                        "sourceErrors": retrieval.get("source_errors") if isinstance(retrieval, dict) else None,
                        "slowSourcesSkipped": staged.get("slow_sources_skipped") if isinstance(staged, dict) else None,
                    }
                )

            telemetry_status, telemetry_payload, telemetry_elapsed_ms = _http_json(
                method="GET",
                url=f"{base_url}/telemetry/retrieval?traffic_class=benchmark",
                payload=None,
                api_key=api_key,
                timeout_secs=max(5.0, float(args.timeout_secs)),
            )
            alerts = telemetry_payload.get("alerts") if isinstance(telemetry_payload, dict) else {}
            latency_sources = (
                telemetry_payload.get("latency", {}).get("sources", {})
                if isinstance(telemetry_payload, dict)
                else {}
            )
            letta_latency = latency_sources.get("letta") if isinstance(latency_sources, dict) else None
            record = {
                "timestamp": _iso_now(),
                "cycle": cycle,
                "modeResults": mode_results,
                "telemetry": {
                    "status": telemetry_status,
                    "elapsedMs": round(telemetry_elapsed_ms, 3),
                    "alertsCount": int(alerts.get("count") or 0) if isinstance(alerts, dict) else 0,
                    "alertCodes": [item.get("code") for item in (alerts.get("active") or []) if isinstance(item, dict)]
                    if isinstance(alerts, dict)
                    else [],
                    "lettaLatency": letta_latency if isinstance(letta_latency, dict) else {},
                },
            }
            handle.write(json.dumps(record, separators=(",", ":"), ensure_ascii=True) + "\n")
            handle.flush()

            elapsed_cycle = time.monotonic() - cycle_started
            sleep_for = max(0.0, float(args.interval_secs) - elapsed_cycle)
            print(
                f"[cycle={cycle}] alerts={record['telemetry']['alertsCount']} "
                f"letta_p95={record['telemetry']['lettaLatency'].get('p95Ms')} "
                f"letta_p99={record['telemetry']['lettaLatency'].get('p99Ms')} "
                f"sleep={round(sleep_for, 2)}s"
            )
            if sleep_for > 0:
                time.sleep(sleep_for)

    summary = {
        "startedAt": started_iso,
        "endedAt": _iso_now(),
        "cycles": cycle,
        "output": str(output_path),
    }
    print(json.dumps(summary))
    return 0


if __name__ == "__main__":
    sys.exit(main())
