from __future__ import annotations

import hashlib
import math
import os
import threading
import time
from datetime import timedelta
from pathlib import Path
from typing import Any

import lancedb
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

from exact_state import ExactStateRegistryError, is_exact_state_path, load_exact_state_paths


PORT = int(os.getenv("PORT", "8097"))
DATA_ROOT = Path(os.getenv("LANCEDB_SPIKE_DATA_ROOT", "/data/memory-bank"))
DB_URI = os.getenv("LANCEDB_SPIKE_DB_URI", "/data/lancedb_spike")
EXACT_STATE_INDEX_PATH = Path(
    os.getenv(
        "SPIKE_EXACT_STATE_INDEX_PATH",
        str(DATA_ROOT / "_contextlattice" / "exact_state_paths.json"),
    )
)
CONTENT_BLOBS_PATH = Path(
    os.getenv(
        "LANCEDB_SPIKE_CONTENT_BLOBS_PATH",
        os.getenv(
            "GO_MEMORY_STORE_CONTENT_BLOBS_PATH",
            str(DATA_ROOT / "_contextlattice" / "content_blobs"),
        ),
    )
)
TABLE_NAME = os.getenv("LANCEDB_SPIKE_TABLE", "memory_bank")
REFRESH_SECS = max(5, int(os.getenv("LANCEDB_SPIKE_REFRESH_SECS", "120")))
MAX_DOCS = max(100, int(os.getenv("LANCEDB_SPIKE_MAX_DOCS", "50000")))
EXACT_STATE_CANDIDATE_MAX = min(MAX_DOCS, 512)
MAX_CONTENT_CHARS = max(512, int(os.getenv("LANCEDB_SPIKE_MAX_CONTENT_CHARS", "4096")))


class SearchRequest(BaseModel):
    query: str
    limit: int = Field(default=10, ge=1, le=100)
    project: str | None = None
    topic_path: str | None = None
    backend: str | None = None


class SearchResult(BaseModel):
    project: str
    file: str
    summary: str
    score: float
    topic_path: str


class SearchResponse(BaseModel):
    backend: str
    results: list[SearchResult]
    meta: dict[str, Any]


app = FastAPI(title="lancedb-spike-adapter", version="0.1.0")
_lock = threading.Lock()
_state: dict[str, Any] = {
    "db": None,
    "table": None,
    "docs_cache": [],
    "docs_loaded": 0,
    "fingerprint": "",
    "last_refresh_unix_secs": 0,
    "last_error": None,
    "fts_enabled": False,
    "refresh_in_progress": False,
}


def _normalize_topic(topic: str) -> str:
    normalized = topic.replace("\\", "/").strip("/")
    parts = [part for part in normalized.split("/") if part]
    return "/".join(parts)


def _derive_topic_path(file_path: str) -> str:
    parent = Path(file_path.replace("\\", "/")).parent.as_posix()
    topic = _normalize_topic(parent)
    return topic if topic else "general"


def _normalize_text(raw: str) -> str:
    collapsed = " ".join(raw.split())
    return collapsed.strip()


def _resolved_scan_target(
    path: Path,
    root_resolved: Path,
    registry_resolved: Path,
    content_blobs_resolved: Path | None,
    exact_state_paths: set[str],
) -> Path | None:
    try:
        target = path.resolve(strict=True)
    except (OSError, RuntimeError):
        return None
    if target == registry_resolved:
        return None
    if target != path.absolute():
        if content_blobs_resolved is None:
            return None
        try:
            target.relative_to(content_blobs_resolved)
        except ValueError:
            return None
        return target
    try:
        target_rel = target.relative_to(root_resolved)
    except ValueError:
        if content_blobs_resolved is None:
            return None
        try:
            target.relative_to(content_blobs_resolved)
        except ValueError:
            return None
        return target

    parts = target_rel.parts
    if len(parts) < 2:
        return None
    if parts[0].lower() == "_contextlattice":
        if content_blobs_resolved is None:
            return None
        try:
            target.relative_to(content_blobs_resolved)
        except ValueError:
            return None
        return target
    project = (parts[0] or "").strip()
    file_name = Path(*parts[1:]).as_posix().strip()
    if is_exact_state_path(exact_state_paths, project, file_name):
        return None
    return target


def _scan_docs() -> tuple[list[dict[str, Any]], str]:
    if DATA_ROOT.is_symlink():
        raise RuntimeError("memory data root must not be a symlink")
    if not DATA_ROOT.exists():
        raise RuntimeError("memory data root is missing")

    exact_state_paths = load_exact_state_paths(EXACT_STATE_INDEX_PATH)
    try:
        root_resolved = DATA_ROOT.resolve(strict=True)
        registry_resolved = EXACT_STATE_INDEX_PATH.resolve(strict=True)
        content_blobs_resolved = (
            CONTENT_BLOBS_PATH.resolve(strict=True) if CONTENT_BLOBS_PATH.exists() else None
        )
    except (OSError, RuntimeError) as exc:
        raise RuntimeError(f"resolve memory scan boundary: {exc}") from exc
    docs: list[dict[str, Any]] = []
    digest = hashlib.sha256()
    for key in sorted(exact_state_paths):
        digest.update(key.encode("utf-8", errors="ignore"))
    scanned = 0
    for path in DATA_ROOT.rglob("*"):
        if not path.is_file():
            continue
        scanned += 1
        if len(docs) >= MAX_DOCS:
            break
        try:
            rel = path.relative_to(DATA_ROOT)
        except ValueError:
            continue
        parts = rel.parts
        if len(parts) < 2:
            continue
        project = (parts[0] or "").strip()
        file_name = Path(*parts[1:]).as_posix().strip()
        if not project or not file_name:
            continue
        if is_exact_state_path(exact_state_paths, project, file_name):
            continue
        target = _resolved_scan_target(
            path,
            root_resolved,
            registry_resolved,
            content_blobs_resolved,
            exact_state_paths,
        )
        if target is None:
            continue
        try:
            text = target.read_text(encoding="utf-8", errors="ignore")
        except Exception:
            continue
        summary = _normalize_text(text)
        if not summary:
            continue
        if len(summary) > MAX_CONTENT_CHARS:
            summary = summary[:MAX_CONTENT_CHARS]
        topic_path = _derive_topic_path(file_name)
        record = {
            "id": f"{project}::{file_name}",
            "project": project,
            "file": file_name,
            "topic_path": topic_path,
            "summary": summary,
            "text": f"{file_name} {topic_path} {summary}",
        }
        digest.update(record["id"].encode("utf-8", errors="ignore"))
        digest.update(str(target.stat().st_mtime_ns).encode("utf-8", errors="ignore"))
        docs.append(record)
    digest.update(str(scanned).encode("utf-8", errors="ignore"))
    return docs, digest.hexdigest()


def _refresh_worker() -> None:
    refreshed_at = int(time.time())
    try:
        docs, fingerprint = _scan_docs()
        with _lock:
            previous_fingerprint = str(_state.get("fingerprint") or "")
            previous_docs_loaded = int(_state.get("docs_loaded") or 0)
            previous_db = _state.get("db")
            previous_table = _state.get("table")
            previous_fts_enabled = bool(_state.get("fts_enabled"))

        if (
            fingerprint
            and fingerprint == previous_fingerprint
            and previous_table is not None
            and previous_docs_loaded == len(docs)
        ):
            # Avoid producing new table/index versions when source files did not change.
            with _lock:
                _state["last_refresh_unix_secs"] = refreshed_at
                _state["last_error"] = None
            return

        db = None
        table = None
        fts_enabled = False
        if docs:
            db = lancedb.connect(DB_URI)
            table = db.create_table(TABLE_NAME, data=docs, mode="overwrite")
            try:
                table.create_fts_index("text", replace=True)
                fts_enabled = True
            except Exception:
                fts_enabled = False
            # Best effort: remove stale versions left by repeated overwrite cycles.
            try:
                cleanup_old_versions = getattr(table, "cleanup_old_versions", None)
                if callable(cleanup_old_versions):
                    try:
                        cleanup_old_versions(older_than=timedelta(seconds=0))
                    except TypeError:
                        cleanup_old_versions()
            except Exception:
                pass
        with _lock:
            _state["docs_cache"] = docs
            _state["docs_loaded"] = len(docs)
            _state["fingerprint"] = fingerprint
            _state["last_refresh_unix_secs"] = refreshed_at
            _state["last_error"] = None
            _state["db"] = db if db is not None else previous_db
            _state["table"] = table if table is not None else previous_table
            _state["fts_enabled"] = fts_enabled if table is not None else previous_fts_enabled
    except Exception as exc:
        with _lock:
            _state["last_error"] = str(exc)
            _state["last_refresh_unix_secs"] = refreshed_at
    finally:
        with _lock:
            _state["refresh_in_progress"] = False


def _trigger_refresh(force: bool = False, wait: bool = False) -> None:
    start_worker = False
    with _lock:
        now = int(time.time())
        last_refresh = int(_state.get("last_refresh_unix_secs") or 0)
        due = bool(force) or (last_refresh <= 0) or ((now - last_refresh) >= REFRESH_SECS)
        if due and not bool(_state.get("refresh_in_progress")):
            _state["refresh_in_progress"] = True
            start_worker = True
    if start_worker:
        if wait:
            _refresh_worker()
        else:
            threading.Thread(target=_refresh_worker, daemon=True).start()
    elif wait:
        deadline = time.time() + 60.0
        while time.time() < deadline:
            with _lock:
                if not bool(_state.get("refresh_in_progress")):
                    break
            time.sleep(0.05)


def _as_rows(query_result: Any) -> list[dict[str, Any]]:
    if query_result is None:
        return []
    if hasattr(query_result, "to_list"):
        return list(query_result.to_list())
    if hasattr(query_result, "to_arrow"):
        return list(query_result.to_arrow().to_pylist())
    if hasattr(query_result, "to_pandas"):
        return list(query_result.to_pandas().to_dict(orient="records"))
    if isinstance(query_result, list):
        return [row for row in query_result if isinstance(row, dict)]
    return []


def _coerce_score(row: dict[str, Any]) -> float:
    raw = row.get("score")
    if raw is None:
        raw = row.get("_score")
    if raw is None:
        raw = row.get("distance")
        if raw is not None:
            try:
                return float(1.0 / (1.0 + float(raw)))
            except Exception:
                return 0.0
    if raw is None:
        return 0.0
    try:
        value = float(raw)
    except Exception:
        return 0.0
    if math.isnan(value) or math.isinf(value):
        return 0.0
    return value


def _tokenize(query: str) -> list[str]:
    cleaned = []
    for ch in query.lower():
        cleaned.append(ch if (ch.isalnum() or ch == "_") else " ")
    return [tok for tok in "".join(cleaned).split() if len(tok) >= 2]


def _lexical_search(query: str, limit: int) -> list[dict[str, Any]]:
    terms = _tokenize(query)
    if not terms:
        return []
    rows: list[dict[str, Any]] = []
    for item in _state.get("docs_cache", []):
        text = str(item.get("text", "")).lower()
        score = 0.0
        for term in terms:
            score += float(text.count(term))
        if score <= 0.0:
            continue
        row = dict(item)
        row["score"] = score
        rows.append(row)
    rows.sort(key=lambda x: float(x.get("score") or 0.0), reverse=True)
    return rows[: max(1, limit)]


def _lancedb_search(query: str, limit: int) -> list[dict[str, Any]]:
    table = _state.get("table")
    if table is None:
        return []
    fetch = min(MAX_DOCS, max(limit * 5, 50))
    try:
        try:
            search_builder = table.search(query, query_type="fts")
        except TypeError:
            search_builder = table.search(query)
        if hasattr(search_builder, "limit"):
            search_builder = search_builder.limit(fetch)
        rows = _as_rows(search_builder)
        if rows:
            return rows
    except Exception as exc:
        _state["last_error"] = str(exc)
    return _lexical_search(query, fetch)


@app.get("/health")
def health() -> dict[str, Any]:
    try:
        exact_state_paths = load_exact_state_paths(EXACT_STATE_INDEX_PATH)
    except ExactStateRegistryError as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc
    _trigger_refresh(force=False, wait=False)
    last_error = _state.get("last_error")
    ready_snapshot = bool(_state.get("fingerprint")) and int(
        _state.get("last_refresh_unix_secs") or 0
    ) > 0
    if last_error and not ready_snapshot:
        raise HTTPException(status_code=503, detail=f"lancedb refresh unavailable: {last_error}")
    return {
        "ok": True,
        "docs_loaded": int(_state.get("docs_loaded") or 0),
        "fingerprint": _state.get("fingerprint") or "",
        "last_refresh_unix_secs": int(_state.get("last_refresh_unix_secs") or 0),
        "fts_enabled": bool(_state.get("fts_enabled")),
        "refresh_in_progress": bool(_state.get("refresh_in_progress")),
        "data_root": str(DATA_ROOT),
        "db_uri": str(DB_URI),
        "table_name": TABLE_NAME,
        "exact_state_paths": len(exact_state_paths),
        "degraded": bool(last_error),
        "last_error": last_error,
    }


@app.post("/search")
def search(req: SearchRequest) -> SearchResponse:
    query = req.query.strip()
    if not query:
        raise HTTPException(status_code=422, detail="query is required")
    _trigger_refresh(force=False, wait=False)
    try:
        selection_paths = load_exact_state_paths(EXACT_STATE_INDEX_PATH)
    except ExactStateRegistryError as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc
    candidate_limit = min(EXACT_STATE_CANDIDATE_MAX, req.limit + len(selection_paths))
    rows = _lancedb_search(query, candidate_limit)
    try:
        exact_state_paths = load_exact_state_paths(EXACT_STATE_INDEX_PATH)
    except ExactStateRegistryError as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc
    project_filter = (req.project or "").strip().lower()
    topic_filter = _normalize_topic(req.topic_path or "")
    def select(candidate_rows: list[dict[str, Any]], exact_paths: set[str]) -> list[SearchResult]:
        selected: list[SearchResult] = []
        for row in candidate_rows:
            project = str(row.get("project") or "").strip()
            file_name = str(row.get("file") or row.get("path") or "").strip()
            summary = _normalize_text(str(row.get("summary") or row.get("text") or row.get("content") or ""))
            topic_path = _normalize_topic(
                str(row.get("topic_path") or row.get("topic") or _derive_topic_path(file_name))
            )
            if project_filter and project.lower() != project_filter:
                continue
            if topic_filter and not topic_path.startswith(topic_filter):
                continue
            if not project or not file_name or not summary:
                continue
            if is_exact_state_path(exact_paths, project, file_name):
                continue
            selected.append(
                SearchResult(
                    project=project,
                    file=file_name,
                    summary=summary,
                    score=_coerce_score(row),
                    topic_path=topic_path or "general",
                )
            )
            if len(selected) >= req.limit:
                break
        return selected

    out = select(rows, exact_state_paths)
    if len(out) < req.limit and exact_state_paths != selection_paths:
        candidate_limit = min(EXACT_STATE_CANDIDATE_MAX, req.limit + len(exact_state_paths))
        rows = _lancedb_search(query, candidate_limit)
        try:
            exact_state_paths = load_exact_state_paths(EXACT_STATE_INDEX_PATH)
        except ExactStateRegistryError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        out = select(rows, exact_state_paths)
    return SearchResponse(
        backend="lancedb_spike",
        results=out,
        meta={
            "docs_loaded": int(_state.get("docs_loaded") or 0),
            "fingerprint": _state.get("fingerprint") or "",
            "fts_enabled": bool(_state.get("fts_enabled")),
            "last_refresh_unix_secs": int(_state.get("last_refresh_unix_secs") or 0),
            "exact_state_paths": len(exact_state_paths),
        },
    )


@app.on_event("startup")
def startup() -> None:
    load_exact_state_paths(EXACT_STATE_INDEX_PATH)
    _trigger_refresh(force=True, wait=True)
    last_error = _state.get("last_error")
    if last_error:
        raise RuntimeError(f"lancedb startup refresh failed: {last_error}")


if __name__ == "__main__":
    import uvicorn

    uvicorn.run("app:app", host="0.0.0.0", port=PORT, reload=False)
