#!/usr/bin/env python3
"""Audit Qdrant + Mongo usage to keep project namespaces tidy.

This script prints per-collection counts from Qdrant and Mongo so we can
spot runaway namespaces (e.g., sol_scaler telemetry overwhelming shared
memory). It is intentionally read-only.
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any, Dict, List

import requests

try:
    from pymongo import MongoClient
except ImportError:  # pragma: no cover - optional dependency
    MongoClient = None  # type: ignore


def fetch_qdrant_stats(base_url: str) -> List[Dict[str, Any]]:
    resp = requests.get(f"{base_url.rstrip('/')}/collections", timeout=10)
    resp.raise_for_status()
    data = resp.json().get("result", {}).get("collections", [])
    stats = []
    for coll in data:
        name = coll["name"]
        detail = requests.get(
            f"{base_url.rstrip('/')}/collections/{name}", timeout=10
        )
        detail.raise_for_status()
        payload = detail.json().get("result", {})
        vectors = payload.get("vectors_count", payload.get("points_count", 0))
        stats.append({"collection": name, "points": vectors})
    return stats


def fetch_mongo_stats(mongo_url: str, db_name: str) -> List[Dict[str, Any]]:
    if MongoClient is None:
        raise RuntimeError(
            "pymongo not installed; run `pip install pymongo` or skip Mongo audit"
        )
    client = MongoClient(mongo_url, serverSelectionTimeoutMS=5000)
    db = client[db_name]
    stats: List[Dict[str, Any]] = []
    for name in db.list_collection_names():
        stats.append({"collection": name, "documents": db[name].estimated_document_count()})
    return stats


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--qdrant-url", default=os.getenv("QDRANT_URL", "http://localhost:6333"))
    parser.add_argument("--mongo-url", default=os.getenv("MONGO_URL", "mongodb://localhost:27017"))
    parser.add_argument("--mongo-db", default=os.getenv("MONGO_DB", "memorybank"))
    parser.add_argument(
        "--json", action="store_true", help="Emit machine-readable JSON instead of tables"
    )
    args = parser.parse_args()

    report: Dict[str, Any] = {}
    try:
        report["qdrant"] = fetch_qdrant_stats(args.qdrant_url)
    except Exception as exc:  # pragma: no cover - diagnostics
        print(f"[warn] Failed to fetch Qdrant stats: {exc}", file=sys.stderr)
    try:
        report["mongo"] = fetch_mongo_stats(args.mongo_url, args.mongo_db)
    except Exception as exc:  # pragma: no cover - diagnostics
        print(f"[warn] Failed to fetch Mongo stats: {exc}", file=sys.stderr)

    if args.json:
        print(json.dumps(report, indent=2, sort_keys=True))
        return

    if report.get("qdrant"):
        print("Qdrant collections:")
        for entry in report["qdrant"]:
            print(f"  - {entry['collection']}: {entry['points']} points")
    if report.get("mongo"):
        print("Mongo collections:")
        for entry in report["mongo"]:
            print(f"  - {entry['collection']}: {entry['documents']} docs")


if __name__ == "__main__":
    main()
