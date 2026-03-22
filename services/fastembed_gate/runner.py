from __future__ import annotations

import os
import subprocess
import time
from datetime import datetime, timezone


def _env(name: str, default: str) -> str:
    value = os.getenv(name, "").strip()
    return value if value else default


def _env_float(name: str, default: float) -> float:
    raw = os.getenv(name, "").strip()
    if not raw:
        return float(default)
    try:
        value = float(raw)
    except ValueError:
        return float(default)
    return float(value)


def _env_int(name: str, default: int) -> int:
    raw = os.getenv(name, "").strip()
    if not raw:
        return int(default)
    try:
        value = int(raw)
    except ValueError:
        return int(default)
    return int(value)


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _build_cmd() -> list[str]:
    base_url = _env("GATE_REFRESH_BASE_URL", "http://contextlattice-orchestrator:8075")
    project = _env("GATE_REFRESH_PROJECT", "contextlattice")
    timeout = _env_float("GATE_REFRESH_TIMEOUT_SECS", 45.0)
    runs = _env_int("GATE_REFRESH_RUNS", 10)
    baseline = _env("GATE_REFRESH_BASELINE", "/app/bench/results/perf_shortlist_matrix_baseline.json")
    output = _env("GATE_REFRESH_OUTPUT", "/app/data/bench/perf_shortlist_matrix_latest.json")
    gate_output = _env("GATE_REFRESH_GATE_OUTPUT", "/app/data/gates/fastembed_gate_latest.json")
    gate_min = _env_float("GATE_REFRESH_GATE_MIN_IMPROVEMENT_PCT", 5.0)
    gate_max_err = _env_float("GATE_REFRESH_GATE_MAX_ERROR_REGRESSION", 0.005)
    gate_warmups = _env_int("GATE_REFRESH_WARMUPS", 1)
    gate_repeats = _env_int("GATE_REFRESH_REPEATS", 3)
    gate_aggregate = _env("GATE_REFRESH_AGGREGATE", "median")
    gate_sleep = _env_float("GATE_REFRESH_GATE_SLEEP_SECS", 1.0)

    cmd = [
        "python3",
        "/app/bench/perf_shortlist_matrix.py",
        "--base-url",
        base_url,
        "--project",
        project,
        "--runs",
        str(max(1, runs)),
        "--timeout",
        str(max(5.0, timeout)),
        "--baseline",
        baseline,
        "--gate-min-improvement-pct",
        str(max(0.0, gate_min)),
        "--gate-max-error-regression",
        str(max(0.0, gate_max_err)),
        "--gate-warmups",
        str(max(0, gate_warmups)),
        "--gate-repeats",
        str(max(1, gate_repeats)),
        "--gate-aggregate",
        gate_aggregate,
        "--gate-sleep-secs",
        str(max(0.0, gate_sleep)),
        "--output",
        output,
        "--gate-output",
        gate_output,
    ]
    api_key = os.getenv("GATE_REFRESH_API_KEY", "").strip()
    if api_key:
        cmd.extend(["--api-key", api_key])
    return cmd


def main() -> int:
    interval_secs = _env_float("GATE_REFRESH_INTERVAL_SECS", 1800.0)
    min_pause_secs = max(15.0, _env_float("GATE_REFRESH_MIN_PAUSE_SECS", 30.0))
    failure_retry_secs = max(15.0, _env_float("GATE_REFRESH_FAILURE_RETRY_SECS", 45.0))
    while True:
        started = time.monotonic()
        cmd = _build_cmd()
        display_cmd = list(cmd)
        for idx, token in enumerate(display_cmd):
            if token == "--api-key" and idx + 1 < len(display_cmd):
                display_cmd[idx + 1] = "<redacted>"
        print(f"[{_now()}] fastembed gate refresh start: {' '.join(display_cmd)}", flush=True)
        success = False
        try:
            proc = subprocess.run(cmd, check=False)
            if proc.returncode == 0:
                success = True
                print(f"[{_now()}] fastembed gate refresh complete: rc=0", flush=True)
            else:
                print(f"[{_now()}] fastembed gate refresh failed: rc={proc.returncode}", flush=True)
        except Exception as exc:  # pragma: no cover
            print(f"[{_now()}] fastembed gate refresh exception: {exc}", flush=True)

        elapsed = time.monotonic() - started
        if success:
            sleep_for = max(min_pause_secs, max(0.0, interval_secs - elapsed))
        else:
            sleep_for = max(min_pause_secs, failure_retry_secs)
        time.sleep(sleep_for)


if __name__ == "__main__":
    raise SystemExit(main())
