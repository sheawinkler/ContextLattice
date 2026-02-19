#!/usr/bin/env python3
"""Garbage-collect fanout_outbox rows in the orchestrator sqlite task DB."""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Iterable


def _utc_now_iso() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _cutoff_iso(hours: int) -> str:
    cutoff = datetime.now(timezone.utc) - timedelta(hours=max(0, int(hours)))
    return cutoff.isoformat().replace("+00:00", "Z")


def _parse_csv(raw: str) -> list[str]:
    return [item.strip() for item in raw.split(",") if item.strip()]


def _resolve_db_path(cli_value: str | None) -> Path:
    candidates: list[Path] = []
    if cli_value:
        candidates.append(Path(cli_value))
    env_task_db = os.getenv("TASK_DB_PATH", "").strip()
    if env_task_db:
        candidates.append(Path(env_task_db))
    orch_data_dir = os.getenv("ORCHESTRATOR_DATA_DIR", "").strip()
    if orch_data_dir:
        candidates.append(Path(orch_data_dir) / "agent_tasks.db")
    candidates.extend(
        [
            Path("/Volumes/wd_black/memmcp/orchestrator/agent_tasks.db"),
            Path("services/orchestrator/data/agent_tasks.db"),
        ]
    )
    for path in candidates:
        if path.exists():
            return path
    # Fall back to first candidate for better error visibility.
    if candidates:
        return candidates[0]
    return Path("services/orchestrator/data/agent_tasks.db")


def _table_exists(conn: sqlite3.Connection, table_name: str) -> bool:
    row = conn.execute(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name=? LIMIT 1;",
        (table_name,),
    ).fetchone()
    return row is not None


def _count_rows(conn: sqlite3.Connection, sql: str, params: Iterable[object] = ()) -> int:
    row = conn.execute(sql, tuple(params)).fetchone()
    return int(row[0]) if row and row[0] is not None else 0


def _status_counts(conn: sqlite3.Connection) -> dict[str, int]:
    rows = conn.execute(
        "SELECT status, COUNT(*) FROM fanout_outbox NOT INDEXED GROUP BY status ORDER BY status ASC;"
    ).fetchall()
    return {str(status): int(count) for status, count in rows}


def main() -> int:
    parser = argparse.ArgumentParser(description="Prune old fanout_outbox rows from sqlite")
    parser.add_argument("--db-path", default=os.getenv("FANOUT_OUTBOX_GC_DB_PATH", "").strip() or None)
    parser.add_argument(
        "--succeeded-retention-hours",
        type=int,
        default=int(os.getenv("FANOUT_OUTBOX_SUCCEEDED_RETENTION_HOURS", "24")),
    )
    parser.add_argument(
        "--failed-retention-hours",
        type=int,
        default=int(os.getenv("FANOUT_OUTBOX_FAILED_RETENTION_HOURS", "168")),
    )
    parser.add_argument(
        "--stale-pending-hours",
        type=int,
        default=int(os.getenv("FANOUT_OUTBOX_STALE_PENDING_HOURS", "24")),
    )
    parser.add_argument(
        "--stale-targets",
        default=os.getenv("FANOUT_OUTBOX_STALE_TARGETS", ""),
        help="Comma-separated targets to prune when pending/retrying/running rows are stale",
    )
    parser.add_argument("--vacuum", action="store_true")
    parser.add_argument(
        "--vacuum-min-deleted",
        type=int,
        default=int(os.getenv("FANOUT_OUTBOX_GC_VACUUM_MIN_DELETED", "500")),
    )
    parser.add_argument("--timeout-secs", type=float, default=float(os.getenv("FANOUT_OUTBOX_GC_TIMEOUT_SECS", "15")))
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    db_path = _resolve_db_path(args.db_path)
    if not db_path.exists():
        print(
            json.dumps(
                {
                    "ok": True,
                    "message": "fanout outbox DB not found; skipping",
                    "db_path": str(db_path),
                    "timestamp": _utc_now_iso(),
                }
            )
        )
        return 0

    timeout_secs = max(1.0, float(args.timeout_secs))
    conn = sqlite3.connect(str(db_path), timeout=timeout_secs)
    conn.execute(f"PRAGMA busy_timeout = {int(timeout_secs * 1000)};")
    conn.execute("PRAGMA journal_mode=WAL;")

    try:
        if not _table_exists(conn, "fanout_outbox"):
            print(
                json.dumps(
                    {
                        "ok": True,
                        "message": "fanout_outbox table missing; skipping",
                        "db_path": str(db_path),
                        "timestamp": _utc_now_iso(),
                    }
                )
            )
            return 0

        try:
            before_total = _count_rows(conn, "SELECT COUNT(*) FROM fanout_outbox NOT INDEXED;")
            before_status = _status_counts(conn)
            stale_targets = _parse_csv(args.stale_targets)

            succeeded_cutoff = _cutoff_iso(args.succeeded_retention_hours)
            failed_cutoff = _cutoff_iso(args.failed_retention_hours)
            pending_cutoff = _cutoff_iso(args.stale_pending_hours)

            delete_succeeded_sql = (
                "DELETE FROM fanout_outbox NOT INDEXED "
                "WHERE status='succeeded' AND COALESCE(completed_at, updated_at, created_at) < ?;"
            )
            delete_failed_sql = (
                "DELETE FROM fanout_outbox NOT INDEXED "
                "WHERE status='failed' AND COALESCE(completed_at, updated_at, created_at) < ?;"
            )

            if args.dry_run:
                succeeded_deleted = _count_rows(
                    conn,
                    "SELECT COUNT(*) FROM fanout_outbox NOT INDEXED WHERE status='succeeded' AND COALESCE(completed_at, updated_at, created_at) < ?;",
                    (succeeded_cutoff,),
                )
                failed_deleted = _count_rows(
                    conn,
                    "SELECT COUNT(*) FROM fanout_outbox NOT INDEXED WHERE status='failed' AND COALESCE(completed_at, updated_at, created_at) < ?;",
                    (failed_cutoff,),
                )
            else:
                cur = conn.execute(delete_succeeded_sql, (succeeded_cutoff,))
                succeeded_deleted = int(cur.rowcount if cur.rowcount is not None else 0)
                cur = conn.execute(delete_failed_sql, (failed_cutoff,))
                failed_deleted = int(cur.rowcount if cur.rowcount is not None else 0)

            stale_deleted = 0
            if stale_targets:
                placeholders = ",".join("?" for _ in stale_targets)
                stale_statuses = ("pending", "retrying", "running")
                stale_sql = (
                    "FROM fanout_outbox NOT INDEXED WHERE "
                    f"target IN ({placeholders}) "
                    f"AND status IN ({','.join('?' for _ in stale_statuses)}) "
                    "AND COALESCE(last_attempt_at, updated_at, created_at) < ?"
                )
                stale_params = tuple(stale_targets) + stale_statuses + (pending_cutoff,)
                if args.dry_run:
                    stale_deleted = _count_rows(conn, f"SELECT COUNT(*) {stale_sql};", stale_params)
                else:
                    cur = conn.execute(f"DELETE {stale_sql};", stale_params)
                    stale_deleted = int(cur.rowcount if cur.rowcount is not None else 0)

            deleted_total = succeeded_deleted + failed_deleted + stale_deleted
            vacuum_ran = False
            checkpoint_ok = True
            checkpoint_error = ""
            vacuum_error = ""

            if args.dry_run:
                conn.rollback()
            else:
                conn.commit()
                try:
                    conn.execute("PRAGMA wal_checkpoint(TRUNCATE);")
                except sqlite3.Error as exc:
                    checkpoint_ok = False
                    checkpoint_error = str(exc)
                if args.vacuum and deleted_total >= max(0, int(args.vacuum_min_deleted)):
                    try:
                        conn.execute("VACUUM;")
                        vacuum_ran = True
                    except sqlite3.Error as exc:
                        vacuum_error = str(exc)

            after_total = _count_rows(conn, "SELECT COUNT(*) FROM fanout_outbox NOT INDEXED;")
            after_status = _status_counts(conn)
            db_size_bytes = db_path.stat().st_size if db_path.exists() else 0

            print(
                json.dumps(
                    {
                        "ok": True,
                        "dry_run": bool(args.dry_run),
                        "db_path": str(db_path),
                        "db_size_bytes": db_size_bytes,
                        "before_total": before_total,
                        "after_total": after_total,
                        "before_status": before_status,
                        "after_status": after_status,
                        "deleted": {
                            "succeeded": succeeded_deleted,
                            "failed": failed_deleted,
                            "stale_pending_targets": stale_deleted,
                            "total": deleted_total,
                        },
                        "retention_hours": {
                            "succeeded": int(args.succeeded_retention_hours),
                            "failed": int(args.failed_retention_hours),
                            "stale_pending": int(args.stale_pending_hours),
                        },
                        "stale_targets": stale_targets,
                        "checkpoint": {"ok": checkpoint_ok, "error": checkpoint_error},
                        "vacuum": {
                            "requested": bool(args.vacuum),
                            "ran": vacuum_ran,
                            "min_deleted": int(args.vacuum_min_deleted),
                            "error": vacuum_error,
                        },
                        "timestamp": _utc_now_iso(),
                    },
                    sort_keys=True,
                )
            )
            return 0
        except sqlite3.DatabaseError as exc:
            conn.rollback()
            print(
                json.dumps(
                    {
                        "ok": False,
                        "db_path": str(db_path),
                        "error": str(exc),
                        "hint": "SQLite integrity issue detected. Consider stopping orchestrator, running REINDEX fanout_outbox indexes, then retry GC.",
                        "timestamp": _utc_now_iso(),
                    },
                    sort_keys=True,
                )
            )
            return 1
    finally:
        conn.close()


if __name__ == "__main__":
    sys.exit(main())
