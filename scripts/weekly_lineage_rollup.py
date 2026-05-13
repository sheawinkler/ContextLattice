#!/usr/bin/env python3
"""Generate weekly project lineage + global synergy rollups.

Design goals:
- Metadata-first snapshots (no raw content duplication).
- Stable week-over-week lineage with previous-week refs.
- Zero-copy optimization: reuse previous topic-count index when unchanged.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import tempfile
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from itertools import combinations
from pathlib import Path
from typing import Any

STOPWORDS = {
    "root",
    "notes",
    "tasks",
    "task",
    "tmp",
    "state",
    "stats",
    "snapshot",
    "snapshots",
    "health",
    "system",
    "data",
    "project",
    "projects",
    "file",
    "files",
    "run",
    "runs",
    "log",
    "logs",
}

SKILL_HINTS = {
    "rust": "rust",
    "go": "go",
    "golang": "go",
    "vector": "vector-search",
    "graph": "graph-reasoning",
    "retrieval": "retrieval-policy",
    "latency": "performance-tuning",
    "telemetry": "observability",
    "security": "security-hardening",
    "billing": "paid-operations",
    "auth": "auth-identity",
    "mcp": "mcp-interoperability",
}


def now_utc() -> datetime:
    return datetime.now(tz=UTC)


def iso_now() -> str:
    return now_utc().isoformat().replace("+00:00", "Z")


def parse_week_id(value: str) -> tuple[int, int]:
    match = re.fullmatch(r"(\d{4})-W(\d{2})", value.strip())
    if not match:
        raise ValueError(f"invalid week id: {value}")
    year = int(match.group(1))
    week = int(match.group(2))
    if week < 1 or week > 53:
        raise ValueError(f"invalid week number in week id: {value}")
    return year, week


def current_week_id(ts: datetime | None = None) -> str:
    ts = ts or now_utc()
    iso = ts.isocalendar()
    return f"{iso.year:04d}-W{iso.week:02d}"


def week_start_from_id(week_id: str) -> datetime:
    year, week = parse_week_id(week_id)
    # Monday start for ISO week.
    dt = datetime.fromisocalendar(year, week, 1)
    return dt.replace(tzinfo=UTC)


def json_sha256(payload: Any) -> str:
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def coerce_int(value: Any, fallback: int = 0) -> int:
    try:
        return int(value)
    except Exception:
        return fallback


def default_memory_root() -> str:
    candidates = [
        os.getenv("GO_MEMORY_STORE_ROOT", "").strip(),
        os.getenv("MEMORY_BANK_ROOT", "").strip(),
    ]
    memory_bank_data = os.getenv("MEMORY_BANK_DATA", "").strip()
    if memory_bank_data:
        candidates.append(str(Path(memory_bank_data) / "memory-bank"))
    candidates.append(os.getenv("CONTEXTLATTICE_MEMORY_ROOT", "").strip())
    for candidate in candidates:
        if candidate:
            return candidate
    return "/tmp/contextlattice-memory-bank"


def default_lineage_root() -> str:
    explicit = os.getenv("CONTEXTLATTICE_LINEAGE_ROOT", "").strip()
    if explicit:
        return explicit
    cold_root = os.getenv("CONTEXTLATTICE_COLD_ROOT", "").strip()
    if cold_root:
        return str(Path(cold_root) / "lineage")
    return "./.data/cold/lineage"


def load_local_dotenv() -> None:
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

    def get_json(self, path: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        params = params or {}
        q = urllib.parse.urlencode({k: v for k, v in params.items() if v is not None and str(v) != ""})
        url = f"{self.base_url.rstrip('/')}/{path.lstrip('/')}"
        if q:
            url += "?" + q
        req = urllib.request.Request(url, headers=self._headers())
        with urllib.request.urlopen(req, timeout=self.timeout_secs) as resp:
            return json.loads(resp.read().decode("utf-8"))

    def _headers(self) -> dict[str, str]:
        headers: dict[str, str] = {"accept": "application/json"}
        if self.api_key:
            headers["x-api-key"] = self.api_key
        return headers


def discover_projects(memory_root: Path) -> list[str]:
    if not memory_root.exists() or not memory_root.is_dir():
        return []
    projects: list[str] = []
    for item in sorted(memory_root.iterdir()):
        if not item.is_dir():
            continue
        name = item.name.strip()
        if not name or name.startswith(".") or name.startswith("_"):
            continue
        projects.append(name)
    return projects


def discover_projects_from_api(client: HTTPClient, limit: int = 1000, max_pages: int = 20) -> list[str]:
    projects: set[str] = set()
    offset = 0
    page = 0
    while page < max_pages:
        payload = client.get_json(
            "/memory/topics/list",
            {
                "limit": max(1, min(5000, int(limit))),
                "offset": max(0, int(offset)),
            },
        )
        topics = payload.get("topics") if isinstance(payload.get("topics"), list) else []
        if not topics:
            break
        for row in topics:
            if not isinstance(row, dict):
                continue
            project = str(row.get("project") or "").strip()
            if project:
                projects.add(project)
        offset += len(topics)
        total = coerce_int(payload.get("total"), 0)
        page += 1
        if total > 0 and offset >= total:
            break
        if len(topics) < limit:
            break
    return sorted(projects)


def fetch_topic_rollups(client: HTTPClient, project: str, min_count: int, limit: int) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    offset = 0
    while True:
        payload = client.get_json(
            "/memory/topic-rollups",
            {
                "project": project,
                "min_count": min_count,
                "limit": limit,
                "offset": offset,
            },
        )
        topics = payload.get("topics") if isinstance(payload.get("topics"), list) else []
        if not topics:
            break
        for raw in topics:
            if isinstance(raw, dict):
                rows.append(raw)
        total = coerce_int(payload.get("total"), len(rows))
        offset += len(topics)
        if offset >= total or len(topics) < limit:
            break
    return rows


def normalize_topics(rows: list[dict[str, Any]]) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for row in rows:
        path = str(row.get("path") or "").strip()
        if not path:
            continue
        out.append(
            {
                "path": path,
                "event_count": coerce_int(row.get("eventCount"), 0),
                "unique_file_count": coerce_int(row.get("uniqueFileCount"), 0),
                "depth": coerce_int(row.get("depth"), 0),
                "latest_timestamp": str(row.get("latestTimestamp") or "").strip() or None,
            }
        )
    out.sort(key=lambda r: (-r["event_count"], r["path"]))
    return out


def topic_count_map(topics: list[dict[str, Any]]) -> dict[str, int]:
    return {row["path"]: int(row["event_count"]) for row in topics}


def load_json(path: Path) -> dict[str, Any] | None:
    if not path.exists() or not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except Exception:
        return None


def write_json_atomic(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    encoded = json.dumps(payload, indent=2, ensure_ascii=False) + "\n"
    with tempfile.NamedTemporaryFile("w", encoding="utf-8", dir=str(path.parent), delete=False) as tmp:
        tmp.write(encoded)
        tmp_path = Path(tmp.name)
    tmp_path.replace(path)


def read_counts_ref(path: Path) -> dict[str, int]:
    payload = load_json(path)
    if not isinstance(payload, dict):
        return {}
    rows = payload.get("counts") if isinstance(payload.get("counts"), list) else []
    out: dict[str, int] = {}
    for row in rows:
        if not (isinstance(row, list) and len(row) == 2):
            continue
        topic = str(row[0]).strip()
        if not topic:
            continue
        out[topic] = coerce_int(row[1], 0)
    return out


def compute_delta(curr: dict[str, int], prev: dict[str, int]) -> dict[str, Any]:
    curr_keys = set(curr)
    prev_keys = set(prev)
    added = curr_keys - prev_keys
    removed = prev_keys - curr_keys
    common = curr_keys & prev_keys

    changed: list[tuple[str, int, int]] = []
    for topic in common:
        c = curr[topic]
        p = prev[topic]
        if c != p:
            changed.append((topic, p, c))
    changed.sort(key=lambda row: abs(row[2] - row[1]), reverse=True)

    return {
        "topic_delta": len(curr_keys) - len(prev_keys),
        "event_count_delta": sum(curr.values()) - sum(prev.values()),
        "added_topics": len(added),
        "removed_topics": len(removed),
        "changed_topics": len(changed),
        "top_changes": [
            {"path": topic, "prev": p, "curr": c, "delta": c - p}
            for topic, p, c in changed[:25]
        ],
    }


def tokenize_topic(path: str) -> set[str]:
    tokens = re.split(r"[^a-zA-Z0-9]+", path.lower())
    out = set()
    for tok in tokens:
        tok = tok.strip()
        if len(tok) < 3 or tok in STOPWORDS:
            continue
        out.add(tok)
    return out


def build_synergy(
    week_id: str,
    project_summaries: list[dict[str, Any]],
    min_projects: int,
) -> dict[str, Any]:
    per_project_counts: dict[str, dict[str, int]] = {}
    for summary in project_summaries:
        project = str(summary.get("project") or "").strip()
        counts_ref = summary.get("counts_ref")
        if not project or not isinstance(counts_ref, str):
            continue
        counts = read_counts_ref(Path(counts_ref))
        if counts:
            per_project_counts[project] = counts

    token_project_weight: dict[str, dict[str, int]] = {}
    for project, counts in per_project_counts.items():
        project_weights: dict[str, int] = {}
        for topic, count in counts.items():
            for tok in tokenize_topic(topic):
                project_weights[tok] = project_weights.get(tok, 0) + int(count)
        for tok, weight in project_weights.items():
            bucket = token_project_weight.setdefault(tok, {})
            bucket[project] = weight

    overlaps: list[dict[str, Any]] = []
    for token, by_project in token_project_weight.items():
        if len(by_project) < max(2, min_projects):
            continue
        overlaps.append(
            {
                "token": token,
                "project_count": len(by_project),
                "total_weight": sum(by_project.values()),
                "projects": [
                    {"project": name, "weight": weight}
                    for name, weight in sorted(by_project.items(), key=lambda item: item[1], reverse=True)
                ],
            }
        )
    overlaps.sort(key=lambda row: (-row["project_count"], -row["total_weight"], row["token"]))

    pairwise: list[dict[str, Any]] = []
    project_names = sorted(per_project_counts.keys())
    for left, right in combinations(project_names, 2):
        left_tokens = set()
        for topic in per_project_counts[left].keys():
            left_tokens.update(tokenize_topic(topic))
        right_tokens = set()
        for topic in per_project_counts[right].keys():
            right_tokens.update(tokenize_topic(topic))
        union = left_tokens | right_tokens
        if not union:
            continue
        inter = left_tokens & right_tokens
        if not inter:
            continue
        jaccard = len(inter) / len(union)
        if jaccard < 0.08:
            continue
        pairwise.append(
            {
                "projects": [left, right],
                "jaccard": round(jaccard, 4),
                "shared_tokens": sorted(inter)[:24],
            }
        )
    pairwise.sort(key=lambda row: row["jaccard"], reverse=True)

    skill_candidates: list[dict[str, Any]] = []
    for row in overlaps[:120]:
        token = row["token"]
        hint = SKILL_HINTS.get(token)
        if not hint:
            continue
        skill_candidates.append(
            {
                "skill": hint,
                "trigger_token": token,
                "project_count": row["project_count"],
                "projects": [item["project"] for item in row["projects"][:8]],
            }
        )

    return {
        "schema_version": 1,
        "generated_at": iso_now(),
        "week_id": week_id,
        "project_count": len(per_project_counts),
        "project_refs": [
            {
                "project": str(summary.get("project") or ""),
                "summary_ref": str(summary.get("summary_path") or ""),
                "counts_ref": str(summary.get("counts_ref") or ""),
                "fingerprint": str(summary.get("fingerprint") or ""),
            }
            for summary in sorted(project_summaries, key=lambda s: str(s.get("project") or ""))
        ],
        "synergy_tokens": overlaps[:200],
        "project_pairwise": pairwise[:80],
        "skill_candidates": skill_candidates,
    }


def find_previous_summary(project_dir: Path, week_id: str) -> Path | None:
    candidates: list[Path] = []
    for path in project_dir.glob("week-*.json"):
        week_token = path.stem.replace("week-", "", 1)
        try:
            parse_week_id(week_token)
        except ValueError:
            continue
        if week_token < week_id:
            candidates.append(path)
    if not candidates:
        return None
    candidates.sort()
    return candidates[-1]


def prune_old_weeks(root: Path, keep_weeks: int) -> None:
    if keep_weeks <= 0 or not root.exists():
        return
    # Keep newest N weekly summaries per project/global folder.
    for directory in [root / "projects", root / "global"]:
        if not directory.exists():
            continue
        for child in directory.rglob("week-*.json"):
            # Collect later by parent folder.
            pass

    by_parent: dict[Path, list[Path]] = {}
    for item in root.rglob("week-*.json"):
        by_parent.setdefault(item.parent, []).append(item)

    for parent, items in by_parent.items():
        items.sort()
        if len(items) <= keep_weeks:
            continue
        for stale in items[: len(items) - keep_weeks]:
            try:
                stale.unlink(missing_ok=True)
            except Exception:
                continue


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
        "--memory-root",
        default=default_memory_root(),
    )
    parser.add_argument(
        "--out-root",
        default=default_lineage_root(),
    )
    parser.add_argument("--week-id", default=current_week_id())
    parser.add_argument("--project", action="append", default=[])
    parser.add_argument("--min-count", type=int, default=1)
    parser.add_argument("--page-limit", type=int, default=2000)
    parser.add_argument("--top-topic-limit", type=int, default=60)
    parser.add_argument("--synergy-min-projects", type=int, default=2)
    parser.add_argument("--keep-weeks", type=int, default=104)
    parser.add_argument("--emit-synergy", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--pretty", action="store_true")
    parser.add_argument("--timeout-secs", type=float, default=25.0)
    return parser


def main() -> int:
    load_local_dotenv()
    args = build_parser().parse_args()
    try:
        parse_week_id(args.week_id)
    except ValueError as exc:
        print(json.dumps({"ok": False, "error": str(exc)}, indent=2))
        return 2

    client = HTTPClient(
        base_url=args.orchestrator_url,
        api_key=(args.api_key or "").strip(),
        timeout_secs=max(2.0, float(args.timeout_secs)),
    )

    memory_root = Path(args.memory_root).expanduser()
    out_root = Path(args.out_root).expanduser()
    explicit_projects = [item.strip() for item in args.project if item.strip()]
    projects = sorted(set(explicit_projects or discover_projects(memory_root)))
    if not projects:
        projects = discover_projects_from_api(client, limit=1000, max_pages=20)

    if not projects:
        print(json.dumps({"ok": False, "error": "no projects discovered; pass --project or set memory root"}, indent=2))
        return 2

    weekly_summaries: list[dict[str, Any]] = []
    for project in projects:
        topic_rows = fetch_topic_rollups(
            client,
            project=project,
            min_count=max(1, int(args.min_count)),
            limit=max(1, min(2000, int(args.page_limit))),
        )
        topics = normalize_topics(topic_rows)
        counts_map = topic_count_map(topics)
        counts_compact = [[path, count] for path, count in sorted(counts_map.items())]
        topic_fingerprint = json_sha256(counts_compact)

        project_root = out_root / "projects" / project
        summary_path = project_root / f"week-{args.week_id}.json"
        counts_root = project_root / "counts"
        counts_path = counts_root / f"week-{args.week_id}.json"

        prev_summary_path = find_previous_summary(project_root, args.week_id)
        prev_summary = load_json(prev_summary_path) if prev_summary_path else None
        prev_counts: dict[str, int] = {}
        prev_week_id = None
        prev_fingerprint = None
        prev_counts_ref: str | None = None
        if isinstance(prev_summary, dict):
            prev_week_id = str(prev_summary.get("week_id") or "").strip() or None
            prev_fingerprint = str(prev_summary.get("fingerprint") or "").strip() or None
            prev_counts_ref = str(prev_summary.get("counts_ref") or "").strip() or None
            if prev_counts_ref:
                prev_counts = read_counts_ref(Path(prev_counts_ref))

        counts_ref_to_use = str(counts_path)
        counts_reused = False
        if prev_fingerprint and prev_fingerprint == topic_fingerprint and prev_counts_ref:
            counts_ref_to_use = prev_counts_ref
            counts_reused = True
        elif not args.dry_run:
            write_json_atomic(
                counts_path,
                {
                    "schema_version": 1,
                    "project": project,
                    "week_id": args.week_id,
                    "generated_at": iso_now(),
                    "fingerprint": topic_fingerprint,
                    "counts": counts_compact,
                },
            )

        delta = compute_delta(counts_map, prev_counts) if prev_counts else {
            "topic_delta": len(counts_map),
            "event_count_delta": sum(counts_map.values()),
            "added_topics": len(counts_map),
            "removed_topics": 0,
            "changed_topics": 0,
            "top_changes": [],
        }

        total_events = sum(counts_map.values())
        top_topics = [
            {
                "path": row["path"],
                "event_count": row["event_count"],
                "unique_file_count": row["unique_file_count"],
                "latest_timestamp": row["latest_timestamp"],
            }
            for row in topics[: max(1, int(args.top_topic_limit))]
        ]

        summary_payload = {
            "schema_version": 1,
            "generated_at": iso_now(),
            "week_id": args.week_id,
            "week_start": week_start_from_id(args.week_id).date().isoformat(),
            "project": project,
            "source": "/memory/topic-rollups",
            "fingerprint": topic_fingerprint,
            "stats": {
                "topic_count": len(counts_map),
                "total_event_count": total_events,
                "max_depth": max((coerce_int(row.get("depth"), 0) for row in topics), default=0),
            },
            "delta": {
                "previous_week_id": prev_week_id,
                "previous_fingerprint": prev_fingerprint,
                **delta,
            },
            "top_topics": top_topics,
            "counts_ref": counts_ref_to_use,
            "counts_reused": counts_reused,
            "previous_summary_ref": str(prev_summary_path) if prev_summary_path else None,
        }

        if not args.dry_run:
            write_json_atomic(summary_path, summary_payload)
        weekly_summaries.append({
            "project": project,
            "summary_path": str(summary_path),
            "counts_ref": counts_ref_to_use,
            "fingerprint": topic_fingerprint,
            "topic_count": len(counts_map),
            "total_event_count": total_events,
        })

    global_path = out_root / "global" / f"week-{args.week_id}.json"
    if args.emit_synergy:
        synergy_payload = build_synergy(
            week_id=args.week_id,
            project_summaries=weekly_summaries,
            min_projects=max(2, int(args.synergy_min_projects)),
        )
        if not args.dry_run:
            write_json_atomic(global_path, synergy_payload)

    if not args.dry_run:
        prune_old_weeks(out_root, keep_weeks=max(1, int(args.keep_weeks)))

    result = {
        "ok": True,
        "week_id": args.week_id,
        "projects": weekly_summaries,
        "synergy_emitted": bool(args.emit_synergy),
        "synergy_path": str(global_path) if args.emit_synergy else None,
        "out_root": str(out_root),
        "dry_run": bool(args.dry_run),
    }
    print(json.dumps(result, indent=2 if args.pretty else None, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
