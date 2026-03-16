#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import httpx

CASES = {
    "short_context": "source quality qdrant topic rollups",
    "deep_read": "deep retrieval stability timeout source quality",
    "ops_focus": "fanout queue pressure deadletters backlog quality",
}


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _probe_health(client: httpx.Client, base_url: str) -> dict[str, Any]:
    for path in ("/health", "/v1/health", "/status", "/"):
        target = f"{base_url.rstrip('/')}{path}"
        started = time.perf_counter()
        try:
            resp = client.get(target)
            elapsed = (time.perf_counter() - started) * 1000.0
            if resp.status_code < 500:
                return {
                    "ok": resp.status_code < 400,
                    "status": int(resp.status_code),
                    "path": path,
                    "latencyMs": round(elapsed, 3),
                }
        except Exception as exc:
            last_error = str(exc)
            continue
    return {"ok": False, "status": 0, "path": None, "latencyMs": 0.0, "error": locals().get("last_error", "unreachable")}


def _probe_search(client: httpx.Client, base_url: str, project: str) -> dict[str, Any]:
    rows: dict[str, Any] = {}
    for case, query in CASES.items():
        payload = {"query": query, "project": project, "k": 5, "limit": 5}
        started = time.perf_counter()
        try:
            resp = client.post(f"{base_url.rstrip('/')}/search", json=payload)
            elapsed = (time.perf_counter() - started) * 1000.0
            entry: dict[str, Any] = {
                "status": int(resp.status_code),
                "latencyMs": round(elapsed, 3),
                "ok": resp.status_code < 400,
                "resultCount": 0,
            }
            if resp.status_code < 400:
                body = resp.json()
                if isinstance(body, dict):
                    results = body.get("results")
                    if isinstance(results, list):
                        entry["resultCount"] = len(results)
                    elif isinstance(body.get("hits"), list):
                        entry["resultCount"] = len(body.get("hits") or [])
                elif isinstance(body, list):
                    entry["resultCount"] = len(body)
            else:
                entry["error"] = resp.text[:180]
            rows[case] = entry
        except Exception as exc:
            elapsed = (time.perf_counter() - started) * 1000.0
            rows[case] = {
                "status": 0,
                "latencyMs": round(elapsed, 3),
                "ok": False,
                "resultCount": 0,
                "error": str(exc),
            }
    return rows


def _candidate_url(arg_value: str, env_keys: list[str]) -> str:
    if arg_value.strip():
        return arg_value.strip()
    for key in env_keys:
        value = str(os.getenv(key, "")).strip()
        if value:
            return value
    return ""


def run(args: argparse.Namespace) -> dict[str, Any]:
    candidates = {
        "lancedb": _candidate_url(args.lancedb_url, ["LANCEDB_SPIKE_URL", "LANCEDB_URL"]),
        "trieve": _candidate_url(args.trieve_url, ["TRIEVE_SPIKE_URL", "TRIEVE_URL"]),
        "helixdb": _candidate_url(args.helixdb_url, ["HELIXDB_SPIKE_URL", "HELIXDB_URL"]),
    }

    out: dict[str, Any] = {
        "generatedAt": _now(),
        "project": args.project,
        "candidates": {},
    }

    with httpx.Client(timeout=float(args.timeout)) as client:
        for name, url in candidates.items():
            if not url:
                out["candidates"][name] = {
                    "status": "skipped_unconfigured",
                    "url": "",
                    "health": None,
                    "cases": {},
                }
                continue
            health = _probe_health(client, url)
            cases = _probe_search(client, url, args.project) if bool(health.get("ok")) else {}
            out["candidates"][name] = {
                "status": "ok" if bool(health.get("ok")) else "unreachable",
                "url": url,
                "health": health,
                "cases": cases,
            }

    return out


def main() -> None:
    parser = argparse.ArgumentParser(description="Probe high-priority external retrieval candidates")
    parser.add_argument("--project", default="perf_backend_lanes")
    parser.add_argument("--timeout", type=float, default=8.0)
    parser.add_argument("--lancedb-url", default="")
    parser.add_argument("--trieve-url", default="")
    parser.add_argument("--helixdb-url", default="")
    parser.add_argument("--output", default="")
    args = parser.parse_args()

    payload = run(args)
    rendered = json.dumps(payload, indent=2, sort_keys=True)
    print(rendered)

    output = args.output or (
        "bench/results/high_priority_candidate_probe_"
        + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        + ".json"
    )
    output_path = Path(output)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    output_path.write_text(rendered + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
