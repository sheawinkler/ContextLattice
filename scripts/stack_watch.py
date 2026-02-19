#!/usr/bin/env python3
import argparse
import json
import os
import time
import urllib.error
import urllib.request
from datetime import datetime


def fetch_json(url: str, headers: dict[str, str]) -> dict | None:
    try:
        req = urllib.request.Request(url, headers=headers)
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = resp.read().decode("utf-8")
            return json.loads(data)
    except (urllib.error.URLError, json.JSONDecodeError, TimeoutError):
        return None


def write_jsonl(path: str, payload: dict) -> None:
    with open(path, "a", encoding="utf-8") as handle:
        handle.write(json.dumps(payload) + "\n")


def main() -> int:
    parser = argparse.ArgumentParser(description="ContextLattice stack watcher")
    parser.add_argument("--url", default=os.getenv("MEMMCP_ORCHESTRATOR_URL", "http://127.0.0.1:8075"))
    parser.add_argument("--interval", type=float, default=float(os.getenv("STACK_WATCH_INTERVAL", "10")))
    parser.add_argument("--out", default=os.getenv("STACK_WATCH_OUT", "./tmp/stack_watch.ndjson"))
    parser.add_argument("--alert-out", default=os.getenv("STACK_ALERT_OUT", "./tmp/stack_alerts.ndjson"))
    parser.add_argument("--api-key", default=os.getenv("MEMMCP_ORCHESTRATOR_API_KEY", ""))
    parser.add_argument("--alert-queue-depth", type=int, default=int(os.getenv("ALERT_QUEUE_DEPTH", "500")))
    parser.add_argument("--alert-service-down", type=int, default=int(os.getenv("ALERT_SERVICE_DOWN", "1")))
    args = parser.parse_args()

    headers = {}
    if args.api_key:
        headers["x-api-key"] = args.api_key

    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    os.makedirs(os.path.dirname(args.alert_out) or ".", exist_ok=True)

    while True:
        timestamp = datetime.utcnow().isoformat() + "Z"
        status = fetch_json(f"{args.url.rstrip('/')}/status", headers)
        metrics = fetch_json(f"{args.url.rstrip('/')}/telemetry/metrics", headers)
        trading = fetch_json(f"{args.url.rstrip('/')}/telemetry/trading", headers)

        snapshot = {
            "timestamp": timestamp,
            "status": status,
            "metrics": metrics,
            "trading": trading,
        }
        write_jsonl(args.out, snapshot)

        alerts = []
        if args.alert_service_down and status and isinstance(status, dict):
            for svc in status.get("services", []):
                if not svc.get("healthy", False):
                    alerts.append({
                        "type": "service_down",
                        "service": svc.get("name"),
                        "detail": svc.get("detail"),
                    })
        if metrics and isinstance(metrics, dict):
            queue_depth = int(metrics.get("queueDepth", 0) or 0)
            if queue_depth >= args.alert_queue_depth:
                alerts.append({
                    "type": "queue_depth",
                    "value": queue_depth,
                    "threshold": args.alert_queue_depth,
                })

        for alert in alerts:
            write_jsonl(args.alert_out, {"timestamp": timestamp, **alert})

        time.sleep(args.interval)


if __name__ == "__main__":
    raise SystemExit(main())
