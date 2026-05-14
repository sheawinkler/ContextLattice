#!/usr/bin/env python3
"""Focused perf harness for context_storage_ops crate.

Runs repeatable command timings over WD Black-backed synthetic data to compare
baseline and post-change latency for:
- ledger
- weekly-lineage (dry-run)
- cold-pack (dry-run)
- cold-tier (dry-run)
- archive-ndjson
- fanout-gc (dry-run)
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import time
from pathlib import Path


def run_case(root: Path, name: str, cmd: str, repeat: int, prep=None) -> dict:
    durations = []
    errors = []
    for run_idx in range(repeat):
        if prep:
            prep(run_idx)
        start = time.perf_counter()
        proc = subprocess.run(cmd, cwd=root, shell=True, capture_output=True, text=True)
        elapsed = time.perf_counter() - start
        durations.append(elapsed)
        if proc.returncode != 0:
            errors.append(
                {
                    "run": run_idx,
                    "code": proc.returncode,
                    "stderr_tail": proc.stderr[-1200:],
                    "stdout_tail": proc.stdout[-600:],
                }
            )
    durations_sorted = sorted(durations)
    p50 = durations_sorted[len(durations_sorted) // 2]
    return {
        "name": name,
        "repeat": repeat,
        "ok": len(errors) == 0,
        "durations_sec": durations,
        "p50_sec": p50,
        "mean_sec": sum(durations) / len(durations),
        "min_sec": min(durations),
        "max_sec": max(durations),
        "errors": errors,
    }


def ensure_dataset(data_dir: Path) -> tuple[Path, Path]:
    cold_root = data_dir / "cold"
    cold_root.mkdir(parents=True, exist_ok=True)
    for bucket in ("contextlattice_notes", "memmcp_notes"):
        (cold_root / bucket).mkdir(parents=True, exist_ok=True)

    existing = list(cold_root.rglob("*.snapshot"))
    if len(existing) < 200:
        for stale in cold_root.rglob("*.snapshot"):
            stale.unlink(missing_ok=True)
        for i in range(200):
            bucket = "contextlattice_notes" if i % 2 == 0 else "memmcp_notes"
            ts = f"2026-04-{(i % 28) + 1:02d}-{(i % 23):02d}-{(i % 59):02d}-{(i * 7 % 59):02d}"
            path = cold_root / bucket / f"rollup-{ts}.snapshot"
            payload = ("x" * ((i % 17 + 1) * 4096)).encode()
            path.write_bytes(payload)

    base_ndjson = data_dir / "telemetry_base.ndjson"
    if not base_ndjson.exists() or base_ndjson.stat().st_size < 5_000_000:
        with base_ndjson.open("w") as handle:
            for i in range(120000):
                if i < 90000:
                    ts = f"2026-01-{(i % 28) + 1:02d}T12:00:00.000Z"
                else:
                    ts = f"2026-05-{(i % 14) + 1:02d}T12:00:00.000Z"
                row = {"timestamp": ts, "idx": i, "msg": "m" * 32}
                handle.write(json.dumps(row) + "\n")

    return cold_root, base_ndjson


def resolve_repo_root(arg_repo_root: str | None) -> Path:
    if arg_repo_root:
        return Path(arg_repo_root).expanduser().resolve()
    return Path(__file__).resolve().parents[1]


def resolve_suite_root(arg_suite_root: str | None, repo_root: Path) -> Path:
    if arg_suite_root:
        return Path(arg_suite_root).expanduser().resolve()
    env_root = os.getenv("CONTEXT_STORAGE_OPS_PERF_SUITE_ROOT", "").strip()
    if env_root:
        return Path(env_root).expanduser().resolve()
    return (repo_root / "tmp" / "storage_ops_perf_suite").resolve()


def default_memory_root(repo_root: Path) -> Path:
    for key in ("GO_MEMORY_STORE_ROOT", "MEMORY_BANK_ROOT"):
        raw = os.getenv(key, "").strip()
        if raw:
            return Path(raw).expanduser().resolve()
    return (repo_root / "services" / "orchestrator" / "data" / "memory-bank").resolve()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo-root", default=None, help="repo root (default: inferred from this script location)")
    parser.add_argument(
        "--suite-root",
        default=None,
        help="perf suite output root (default: $CONTEXT_STORAGE_OPS_PERF_SUITE_ROOT or repo/tmp/storage_ops_perf_suite)",
    )
    parser.add_argument("--label", default="run")
    parser.add_argument("--orchestrator-url", default="http://127.0.0.1:8075")
    parser.add_argument("--memory-root", default=None, help="memory bank root path for weekly-lineage source files")
    args = parser.parse_args()

    root = resolve_repo_root(args.repo_root)
    suite_root = resolve_suite_root(args.suite_root, root)
    memory_root = Path(args.memory_root).expanduser().resolve() if args.memory_root else default_memory_root(root)
    data_dir = suite_root / "data"
    results_dir = suite_root / "results"
    data_dir.mkdir(parents=True, exist_ok=True)
    results_dir.mkdir(parents=True, exist_ok=True)

    cold_root, base_ndjson = ensure_dataset(data_dir)
    telemetry_work = data_dir / "telemetry_work.ndjson"
    ledger_out = data_dir / "ledger.ndjson"
    lineage_out = data_dir / "lineage_out"
    cold_archive = data_dir / "cold_archive"

    def prep_telemetry(_):
        shutil.copy2(base_ndjson, telemetry_work)

    runs = []
    runs.append(
        run_case(
            root,
            "ledger",
            f"./scripts/context_storage_ops.sh ledger --orchestrator-url {args.orchestrator_url} --out {ledger_out} --timeout-secs 20 --keep-days 180 --max-bytes 134217728 --tracked-top-limit 24",
            repeat=5,
        )
    )
    runs.append(
        run_case(
            root,
            "weekly_lineage_dry",
            f"./scripts/context_storage_ops.sh weekly-lineage --orchestrator-url {args.orchestrator_url} --memory-root {memory_root} --out-root {lineage_out} --project contextlattice --min-count 1 --page-limit 500 --top-topic-limit 40 --synergy-min-projects 2 --keep-weeks 104 --emit-synergy --dry-run --timeout-secs 25",
            repeat=3,
        )
    )
    runs.append(
        run_case(
            root,
            "cold_pack_dry",
            f"./scripts/context_storage_ops.sh cold-pack --cold-root {cold_root} --level 3 --max-files 0 --catalog _index/cold_snapshot_catalog.jsonl",
            repeat=5,
        )
    )
    runs.append(
        run_case(
            root,
            "cold_tier_dry",
            f"./scripts/context_storage_ops.sh cold-tier --cold-root {cold_root} --keep-latest 6 --keep-daily 21 --keep-weekly 12 --keep-monthly 12",
            repeat=5,
        )
    )
    runs.append(
        run_case(
            root,
            "archive_ndjson",
            f"./scripts/context_storage_ops.sh archive-ndjson --file {telemetry_work} --retention-hours 24 --cold-dir {cold_archive} --timestamp-field timestamp",
            repeat=5,
            prep=prep_telemetry,
        )
    )
    runs.append(
        run_case(
            root,
            "fanout_gc_dry",
            "./scripts/context_storage_ops.sh fanout-gc --dry-run --timeout-secs 15 --succeeded-retention-hours 24 --failed-retention-hours 168 --stale-pending-hours 24",
            repeat=5,
        )
    )

    payload = {
        "captured_at": time.time(),
        "label": args.label,
        "runs": runs,
    }
    out_path = results_dir / f"{args.label}.json"
    out_path.write_text(json.dumps(payload, indent=2))
    print(out_path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
