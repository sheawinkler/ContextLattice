#!/usr/bin/env python3
import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime


RESET = "\033[0m"
FG_GREEN = "\033[32m"
FG_RED = "\033[31m"
FG_YELLOW = "\033[33m"
FG_CYAN = "\033[36m"
FG_MAGENTA = "\033[35m"
FG_DIM = "\033[2m"


def supports_color(no_color: bool) -> bool:
    return sys.stdout.isatty() and not no_color


def colorize(text: str, color: str, enabled: bool) -> str:
    if not enabled:
        return text
    return f"{color}{text}{RESET}"


def fetch_json(url: str, headers: dict[str, str]) -> dict | None:
    try:
        req = urllib.request.Request(url, headers=headers)
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = resp.read().decode("utf-8")
            return json.loads(data)
    except (urllib.error.URLError, json.JSONDecodeError, TimeoutError):
        return None


def bar(value: float, maximum: float, width: int = 24) -> str:
    if maximum <= 0:
        return "[" + ("-" * width) + "]"
    ratio = min(max(value / maximum, 0), 1)
    filled = int(ratio * width)
    return "[" + ("#" * filled) + ("-" * (width - filled)) + "]"


def fmt_time(value: str | None) -> str:
    if not value:
        return "n/a"
    return value


def render(status: dict | None, metrics: dict | None, trading: dict | None, use_color: bool, history: list[int]) -> str:
    lines: list[str] = []
    now = datetime.utcnow().strftime("%Y-%m-%d %H:%M:%SZ")
    lines.append(colorize("ContextLattice Terminal Dashboard", FG_CYAN, use_color))
    lines.append(colorize(f"UTC {now}", FG_DIM, use_color))
    lines.append("")

    if status is None:
        lines.append(colorize("Status: unavailable", FG_RED, use_color))
    else:
        lines.append(colorize("Services", FG_MAGENTA, use_color))
        services = status.get("services", []) if isinstance(status, dict) else []
        if not services:
            lines.append("  (no services reported)")
        for svc in services:
            name = svc.get("name", "unknown")
            healthy = bool(svc.get("healthy"))
            detail = svc.get("detail", "")
            label = "[OK]" if healthy else "[DOWN]"
            color = FG_GREEN if healthy else FG_RED
            lines.append(f"  {colorize(label, color, use_color)} {name:12} {detail}")
    lines.append("")

    lines.append(colorize("Telemetry", FG_MAGENTA, use_color))
    if metrics is None:
        lines.append(colorize("  metrics unavailable", FG_RED, use_color))
    else:
        queue_depth = metrics.get("queueDepth", 0) or 0
        batch_size = metrics.get("batchSize", 0) or 0
        totals = metrics.get("totals", {}) or {}
        lines.append(f"  queueDepth: {queue_depth:5} {bar(queue_depth, max(1, max(history, default=queue_depth)), 18)}")
        lines.append(f"  batchSize : {batch_size:5}")
        lines.append(
            f"  totals    : enq {totals.get('enqueued', 0)} | dropped {totals.get('dropped', 0)} | "
            f"batches {totals.get('batches', 0)} | flushed {totals.get('flushedEvents', 0)}"
        )
    lines.append("")

    lines.append(colorize("Trading", FG_MAGENTA, use_color))
    if trading is None:
        lines.append(colorize("  trading metrics unavailable", FG_YELLOW, use_color))
    else:
        lines.append(f"  updatedAt     : {fmt_time(trading.get('updatedAt'))}")
        lines.append(f"  openPositions : {trading.get('openPositions', 0)}")
        lines.append(f"  totalValueUsd : {trading.get('totalValueUsd', 0.0)}")
        lines.append(f"  realizedPnl   : {trading.get('realizedPnl', 0.0)}")
        lines.append(f"  unrealizedPnl : {trading.get('unrealizedPnl', 0.0)}")

    return "\n".join(lines)


def main() -> int:
    parser = argparse.ArgumentParser(description="ContextLattice terminal status dashboard")
    parser.add_argument("--url", default=os.getenv("MEMMCP_ORCHESTRATOR_URL", "http://127.0.0.1:8075"))
    parser.add_argument("--interval", type=float, default=float(os.getenv("DASHBOARD_INTERVAL", "5")))
    parser.add_argument("--no-color", action="store_true")
    parser.add_argument("--once", action="store_true")
    parser.add_argument("--api-key", default=os.getenv("MEMMCP_ORCHESTRATOR_API_KEY", ""))
    args = parser.parse_args()

    use_color = supports_color(args.no_color)
    headers = {}
    if args.api_key:
        headers["x-api-key"] = args.api_key

    history: list[int] = []
    spinner = ["-", "\\", "|", "/"]
    tick = 0

    while True:
        status = fetch_json(f"{args.url.rstrip('/')}/status", headers)
        metrics = fetch_json(f"{args.url.rstrip('/')}/telemetry/metrics", headers)
        trading = fetch_json(f"{args.url.rstrip('/')}/telemetry/trading", headers)
        if metrics and isinstance(metrics, dict):
            history.append(int(metrics.get("queueDepth", 0) or 0))
            history = history[-20:]

        sys.stdout.write("\033[2J\033[H")
        sys.stdout.write(render(status, metrics, trading, use_color, history))
        sys.stdout.write("\n\n")
        sys.stdout.write(colorize(f"refresh {spinner[tick % len(spinner)]} every {args.interval}s", FG_DIM, use_color))
        sys.stdout.flush()

        if args.once:
            break
        tick += 1
        time.sleep(args.interval)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
