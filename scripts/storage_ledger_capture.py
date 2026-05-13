#!/usr/bin/env python3
"""Capture lightweight storage telemetry snapshots into an append-only ledger.

Purpose:
- Track disk/data growth over time without copying raw payload data.
- Keep only metadata (bytes/counters/ratios), never content bodies.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any


def _now_iso() -> str:
    return datetime.now(tz=UTC).isoformat().replace("+00:00", "Z")


def _parse_iso(value: str) -> datetime | None:
    raw = (value or "").strip()
    if not raw:
        return None
    try:
        if raw.endswith("Z"):
            raw = raw[:-1] + "+00:00"
        dt = datetime.fromisoformat(raw)
        if dt.tzinfo is None:
            return dt.replace(tzinfo=UTC)
        return dt.astimezone(UTC)
    except ValueError:
        return None


def _coerce_int(value: Any, fallback: int = 0) -> int:
    try:
        return int(value)
    except Exception:
        return fallback


def _coerce_float(value: Any, fallback: float = 0.0) -> float:
    try:
        return float(value)
    except Exception:
        return fallback


def _default_ledger_path() -> str:
    explicit = os.getenv("ORCH_STORAGE_LEDGER_PATH", "").strip()
    if explicit:
        return explicit
    go_root = os.getenv("GO_MEMORY_STORE_ROOT", "").strip()
    if go_root:
        return str(Path(go_root) / "_contextlattice" / "storage_ledger.ndjson")
    memory_bank_data = os.getenv("MEMORY_BANK_DATA", "").strip()
    if memory_bank_data:
        return str(Path(memory_bank_data) / "memory-bank" / "_contextlattice" / "storage_ledger.ndjson")
    cold_root = os.getenv("CONTEXTLATTICE_COLD_ROOT", "").strip()
    if cold_root:
        return str(Path(cold_root) / "storage" / "storage_ledger.ndjson")
    return "./.data/orchestrator/storage_ledger.ndjson"


def _load_local_dotenv() -> None:
    env_path = Path(__file__).resolve().parents[1] / ".env"
    if not env_path.exists() or not env_path.is_file():
        return
    key_re = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
    for raw_line in env_path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        value = value.strip()
        if not key_re.match(key):
            continue
        if key in os.environ:
            continue
        if len(value) >= 2 and ((value[0] == value[-1]) and value[0] in {'"', "'"}):
            value = value[1:-1]
        os.environ[key] = value


@dataclass
class HTTPClient:
    base_url: str
    api_key: str
    timeout_secs: float

    def get_json(self, path: str) -> dict[str, Any]:
        url = f"{self.base_url.rstrip('/')}/{path.lstrip('/')}"
        req = urllib.request.Request(url, headers=self._headers())
        with urllib.request.urlopen(req, timeout=self.timeout_secs) as resp:
            payload = resp.read().decode("utf-8")
            return json.loads(payload)

    def _headers(self) -> dict[str, str]:
        headers: dict[str, str] = {"accept": "application/json"}
        if self.api_key:
            headers["x-api-key"] = self.api_key
        return headers


def _extract_tracked_rows(tracked: dict[str, Any], top_limit: int) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for name, raw in tracked.items():
        if name == "_total" or not isinstance(raw, dict):
            continue
        rows.append(
            {
                "name": name,
                "bytes": _coerce_int(raw.get("bytes"), 0),
                "exists": bool(raw.get("exists")),
                "truncated": bool(raw.get("truncated")),
                "error": str(raw.get("error") or "").strip() or None,
            }
        )
    rows.sort(key=lambda row: row["bytes"], reverse=True)
    if top_limit > 0:
        rows = rows[:top_limit]
    return rows


def build_snapshot(
    storage_payload: dict[str, Any],
    status_payload: dict[str, Any],
    orchestrator_url: str,
    tracked_top_limit: int,
) -> dict[str, Any]:
    disk = storage_payload.get("disk") if isinstance(storage_payload.get("disk"), dict) else {}
    tracked = (
        storage_payload.get("trackedArtifacts")
        if isinstance(storage_payload.get("trackedArtifacts"), dict)
        else {}
    )
    storage_governance = (
        storage_payload.get("storageGovernance")
        if isinstance(storage_payload.get("storageGovernance"), dict)
        else {}
    )
    tracked_total = tracked.get("_total") if isinstance(tracked.get("_total"), dict) else {}
    services = status_payload.get("services") if isinstance(status_payload.get("services"), list) else []
    healthy_services = sum(1 for svc in services if isinstance(svc, dict) and bool(svc.get("healthy")))

    return {
        "captured_at": _now_iso(),
        "schema_version": 1,
        "source": "telemetry/storage",
        "orchestrator_url": orchestrator_url,
        "service_health": {
            "healthy": healthy_services,
            "total": len(services),
        },
        "storage": {
            "pressure_band": str(storage_governance.get("pressureBand") or "unknown"),
            "disk": {
                "root": str(disk.get("root") or ""),
                "used_ratio": _coerce_float(disk.get("usedRatio"), 0.0),
                "used_bytes": _coerce_int(disk.get("usedBytes"), 0),
                "free_bytes": _coerce_int(disk.get("freeBytes"), 0),
                "total_bytes": _coerce_int(disk.get("totalBytes"), 0),
            },
            "tracked": {
                "total_bytes": _coerce_int(tracked_total.get("bytes"), 0),
                "top_artifacts": _extract_tracked_rows(tracked, tracked_top_limit),
            },
        },
    }


def _prune_lines(
    lines: list[str],
    keep_days: int,
    max_bytes: int,
) -> list[str]:
    now = datetime.now(tz=UTC)
    cutoff = now - timedelta(days=max(1, keep_days))

    kept: list[str] = []
    for line in lines:
        raw = line.strip()
        if not raw:
            continue
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError:
            continue
        ts = _parse_iso(str(payload.get("captured_at") or payload.get("timestamp") or ""))
        if ts is None or ts >= cutoff:
            kept.append(raw)

    if max_bytes > 0:
        size = sum(len(item.encode("utf-8")) + 1 for item in kept)
        if size > max_bytes:
            trimmed: list[str] = []
            running = 0
            for raw in reversed(kept):
                row_size = len(raw.encode("utf-8")) + 1
                if running + row_size > max_bytes:
                    break
                trimmed.append(raw)
                running += row_size
            kept = list(reversed(trimmed))

    return kept


def prune_ledger(path: Path, keep_days: int, max_bytes: int) -> dict[str, Any]:
    if not path.exists():
        return {"pruned": False, "reason": "missing"}

    original = path.read_text(encoding="utf-8").splitlines()
    kept = _prune_lines(original, keep_days=keep_days, max_bytes=max_bytes)
    if kept == [line.strip() for line in original if line.strip()]:
        return {"pruned": False, "reason": "no_change", "lines": len(kept)}

    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile("w", encoding="utf-8", dir=str(path.parent), delete=False) as tmp:
        tmp_path = Path(tmp.name)
        for raw in kept:
            tmp.write(raw)
            tmp.write("\n")
    tmp_path.replace(path)
    return {
        "pruned": True,
        "lines": len(kept),
        "dropped": max(0, len(original) - len(kept)),
    }


def append_snapshot(path: Path, snapshot: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(snapshot, separators=(",", ":"), ensure_ascii=False))
        handle.write("\n")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--orchestrator-url",
        default=os.getenv(
            "CONTEXTLATTICE_ORCHESTRATOR_URL",
            os.getenv("MEMMCP_ORCHESTRATOR_URL", "http://127.0.0.1:8075"),
        ),
    )
    parser.add_argument(
        "--api-key",
        default=os.getenv(
            "CONTEXTLATTICE_ORCHESTRATOR_API_KEY",
            os.getenv("MEMMCP_ORCHESTRATOR_API_KEY", ""),
        ),
    )
    parser.add_argument(
        "--out",
        default=_default_ledger_path(),
    )
    parser.add_argument("--timeout-secs", type=float, default=float(os.getenv("ORCH_STORAGE_LEDGER_TIMEOUT_SECS", "20")))
    parser.add_argument("--keep-days", type=int, default=int(os.getenv("ORCH_STORAGE_LEDGER_KEEP_DAYS", "180")))
    parser.add_argument("--max-bytes", type=int, default=int(os.getenv("ORCH_STORAGE_LEDGER_MAX_BYTES", str(128 * 1024 * 1024))))
    parser.add_argument("--tracked-top-limit", type=int, default=int(os.getenv("ORCH_STORAGE_LEDGER_TRACKED_TOP_LIMIT", "24")))
    parser.add_argument("--prune-only", action="store_true")
    parser.add_argument("--pretty", action="store_true")
    return parser


def main() -> int:
    _load_local_dotenv()
    args = build_parser().parse_args()
    out_path = Path(args.out).expanduser()

    prune_result = prune_ledger(out_path, keep_days=args.keep_days, max_bytes=args.max_bytes)
    if args.prune_only:
        print(json.dumps({"ok": True, "path": str(out_path), "prune": prune_result}, indent=2 if args.pretty else None))
        return 0

    client = HTTPClient(
        base_url=args.orchestrator_url,
        api_key=(args.api_key or "").strip(),
        timeout_secs=max(2.0, float(args.timeout_secs)),
    )

    try:
        storage_payload = client.get_json("/telemetry/storage")
        status_payload = client.get_json("/status")
    except urllib.error.HTTPError as exc:
        print(json.dumps({"ok": False, "error": f"http_error:{exc.code}", "detail": str(exc)}, indent=2))
        return 2
    except urllib.error.URLError as exc:
        print(json.dumps({"ok": False, "error": "url_error", "detail": str(exc)}, indent=2))
        return 2
    except TimeoutError as exc:
        print(json.dumps({"ok": False, "error": "timeout", "detail": str(exc)}, indent=2))
        return 2

    snapshot = build_snapshot(
        storage_payload=storage_payload,
        status_payload=status_payload,
        orchestrator_url=args.orchestrator_url,
        tracked_top_limit=max(0, int(args.tracked_top_limit)),
    )
    append_snapshot(out_path, snapshot)

    # Re-prune after append to enforce hard cap.
    prune_result = prune_ledger(out_path, keep_days=args.keep_days, max_bytes=args.max_bytes)

    payload = {
        "ok": True,
        "path": str(out_path),
        "captured_at": snapshot["captured_at"],
        "pressure_band": snapshot["storage"]["pressure_band"],
        "disk_used_ratio": snapshot["storage"]["disk"]["used_ratio"],
        "tracked_total_bytes": snapshot["storage"]["tracked"]["total_bytes"],
        "service_health": snapshot["service_health"],
        "prune": prune_result,
    }
    print(json.dumps(payload, indent=2 if args.pretty else None, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
