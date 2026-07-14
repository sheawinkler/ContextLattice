from __future__ import annotations

import hashlib
import math
import os
import threading
import time
from pathlib import Path
from typing import Any

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

from exact_state import ExactStateRegistryError, is_exact_state_path, load_exact_state_paths


PORT = int(os.getenv("PORT", "8098"))
ADAPTER_NAME = os.getenv("SPIKE_ADAPTER_NAME", "external_spike").strip() or "external_spike"
DATA_ROOT = Path(os.getenv("SPIKE_DATA_ROOT", "/data/memory-bank"))
EXACT_STATE_INDEX_PATH = Path(
    os.getenv(
        "SPIKE_EXACT_STATE_INDEX_PATH",
        str(DATA_ROOT / "_contextlattice" / "exact_state_paths.json"),
    )
)
CONTENT_BLOBS_PATH = Path(
    os.getenv(
        "SPIKE_CONTENT_BLOBS_PATH",
        os.getenv(
            "GO_MEMORY_STORE_CONTENT_BLOBS_PATH",
            str(DATA_ROOT / "_contextlattice" / "content_blobs"),
        ),
    )
)
REFRESH_SECS = max(5, int(os.getenv("SPIKE_REFRESH_SECS", "120")))
MAX_DOCS = max(100, int(os.getenv("SPIKE_MAX_DOCS", "50000")))
MAX_CONTENT_CHARS = max(512, int(os.getenv("SPIKE_MAX_CONTENT_CHARS", "4096")))


class SearchRequest(BaseModel):
    query: str
    limit: int = Field(default=10, ge=1, le=100)
    project: str | None = None
    topic_path: str | None = None


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


app = FastAPI(title=f"{ADAPTER_NAME}-adapter", version="0.1.0")
_lock = threading.Lock()
_state: dict[str, Any] = {
    "docs_cache": [],
    "docs_loaded": 0,
    "fingerprint": "",
    "last_refresh_unix_secs": 0,
    "last_error": None,
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
            "text": f"{file_name} {topic_path} {summary}".lower(),
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
            _state["docs_cache"] = docs
            _state["docs_loaded"] = len(docs)
            _state["fingerprint"] = fingerprint
            _state["last_refresh_unix_secs"] = refreshed_at
            _state["last_error"] = None
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


def _tokenize(query: str) -> list[str]:
    cleaned = []
    for ch in query.lower():
        cleaned.append(ch if (ch.isalnum() or ch == "_") else " ")
    return [tok for tok in "".join(cleaned).split() if len(tok) >= 2]


def _coerce_score(value: float) -> float:
    if math.isnan(value) or math.isinf(value):
        return 0.0
    return float(value)


def _search_docs(
    query: str,
    limit: int,
    project_filter: str,
    topic_filter: str,
    exact_state_paths: set[str],
) -> list[SearchResult]:
    terms = _tokenize(query)
    if not terms:
        return []
    rows: list[SearchResult] = []
    docs = list(_state.get("docs_cache") or [])
    for item in docs:
        project = str(item.get("project") or "").strip()
        file_name = str(item.get("file") or "").strip()
        topic_path = _normalize_topic(str(item.get("topic_path") or ""))
        summary = _normalize_text(str(item.get("summary") or ""))
        if not project or not file_name or not summary:
            continue
        if project_filter and project.lower() != project_filter:
            continue
        if topic_filter and not topic_path.startswith(topic_filter):
            continue
        if is_exact_state_path(exact_state_paths, project, file_name):
            continue
        text = str(item.get("text") or "")
        score = 0.0
        for term in terms:
            score += float(text.count(term))
        if score <= 0.0:
            continue
        rows.append(
            SearchResult(
                project=project,
                file=file_name,
                summary=summary,
                score=_coerce_score(score),
                topic_path=topic_path or "general",
            )
        )
    rows.sort(key=lambda row: row.score, reverse=True)
    return rows[: max(1, limit)]


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
        raise HTTPException(status_code=503, detail=f"{ADAPTER_NAME} refresh unavailable: {last_error}")
    return {
        "ok": True,
        "adapter": ADAPTER_NAME,
        "docs_loaded": int(_state.get("docs_loaded") or 0),
        "fingerprint": _state.get("fingerprint") or "",
        "last_refresh_unix_secs": int(_state.get("last_refresh_unix_secs") or 0),
        "refresh_in_progress": bool(_state.get("refresh_in_progress")),
        "data_root": str(DATA_ROOT),
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
    project_filter = (req.project or "").strip().lower()
    topic_filter = _normalize_topic(req.topic_path or "")
    try:
        selection_paths = load_exact_state_paths(EXACT_STATE_INDEX_PATH)
    except ExactStateRegistryError as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc
    rows = _search_docs(query, req.limit, project_filter, topic_filter, selection_paths)
    try:
        exact_state_paths = load_exact_state_paths(EXACT_STATE_INDEX_PATH)
    except ExactStateRegistryError as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc
    rows = [row for row in rows if not is_exact_state_path(exact_state_paths, row.project, row.file)]
    if len(rows) < req.limit and exact_state_paths != selection_paths:
        rows = _search_docs(query, req.limit, project_filter, topic_filter, exact_state_paths)
        try:
            exact_state_paths = load_exact_state_paths(EXACT_STATE_INDEX_PATH)
        except ExactStateRegistryError as exc:
            raise HTTPException(status_code=503, detail=str(exc)) from exc
        rows = [row for row in rows if not is_exact_state_path(exact_state_paths, row.project, row.file)]
    rows = rows[: req.limit]
    return SearchResponse(
        backend=ADAPTER_NAME,
        results=rows,
        meta={
            "adapter": ADAPTER_NAME,
            "exact_state_paths": len(exact_state_paths),
            "docs_loaded": int(_state.get("docs_loaded") or 0),
            "fingerprint": _state.get("fingerprint") or "",
            "last_refresh_unix_secs": int(_state.get("last_refresh_unix_secs") or 0),
        },
    )


@app.on_event("startup")
def startup() -> None:
    load_exact_state_paths(EXACT_STATE_INDEX_PATH)
    _trigger_refresh(force=True, wait=True)
    last_error = _state.get("last_error")
    if last_error:
        raise RuntimeError(f"{ADAPTER_NAME} startup refresh failed: {last_error}")


if __name__ == "__main__":
    import uvicorn

    uvicorn.run("app:app", host="0.0.0.0", port=PORT, reload=False)
