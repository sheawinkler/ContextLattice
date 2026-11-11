"""ContextLattice memory provider for Hermes.

Replaces the shell-hook seed retrieval with the native MemoryProvider lifecycle:
  prefetch          -> scoped /memory/search, injected into the user message (cache-safe)
  on_pre_compress   -> handoff checkpoint written BEFORE the lossy rewrite (no shell hook exists)
  on_session_switch -> next prefetch also pulls the compaction handoff
  on_memory_write   -> every built-in memory add/replace/remove mirrored to CL; a failed
                       mirror goes to a local outbox that is drained on the next start
  tools             -> cl_search (drains continuation_async) / cl_write (verified readback)
  pre_tool_call     -> delegate_task children (skip_memory=True) get one ContextLattice
                       context pack per task (/memory/context-pack, goal-seeded, token
                       budgeted) appended to `context`; falls back to /memory/search
  post_tool_call    -> session observations bind host status and delivered-context digests;
                       never infer first-pass success from a child's completion message
  /cl-reconcile     -> diff MEMORY.md + USER.md against their CL mirrors and backfill gaps
                       (also runs in the background once per `reconcile_hours`)

Failure mode: CL unreachable => explicit DEGRADED line, never silent empty recall.
MEMORY.md / USER.md (built-in) stay local and always loaded.
Config (config.yaml memory.contextlattice): url, project, env_file, limit, mode, timeout,
  delegation_recall, delegation_tokens, delegation_limit, delegation_max_chars,
  drain_seconds, reconcile_hours.
Env: CONTEXTLATTICE_ORCHESTRATOR_URL overrides url.
API key: CONTEXTLATTICE_ORCHESTRATOR_API_KEY from env, Hermes secrets, or env_file.
"""
from __future__ import annotations

import datetime as _dt
import contextlib
import hashlib
import importlib
import json
import logging
import os
import re
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

try:
    import fcntl
except ImportError:
    fcntl = None

from agent.memory_provider import MemoryProvider, spawn_context_thread

logger = logging.getLogger(__name__)

_DEFAULTS: Dict[str, Any] = {
    "url": "http://127.0.0.1:8075",
    "project": "hermes-agent",
    "env_file": "",  # optional path to a .env holding CONTEXTLATTICE_ORCHESTRATOR_API_KEY
    "limit": 5,
    "mode": "fast",
    "timeout": 6.0,
    "delegation_recall": True,
    "delegation_tokens": 1200,     # target_context_pack_tokens per child
    "delegation_limit": 6,
    "delegation_max_chars": 6000,  # hard cap on the injected block
    "drain_seconds": 8.0,          # cl_search continuation drain budget
    "reconcile_hours": 24,
}
_KEY_NAME = "CONTEXTLATTICE_ORCHESTRATOR_API_KEY"
_HANDOFF_TOPIC = "runbooks/context-compaction-handoff"
_ENTRY_DELIMITER = "\n§\n"  # tools/memory_tool_store.py ENTRY_DELIMITER
_OUTBOX_MAX_ITEMS = 256
_OUTBOX_MAX_BYTES = 2 * 1024 * 1024
_OUTBOX_MAX_ITEM_BYTES = 256 * 1024
_OUTBOX_DRAIN_BATCH = 32
_DELEGATION_MAX_TASKS = 8
_DELEGATION_MAX_PENDING = 128
_DELEGATION_TTL = 3600.0
_PACK_HEADER = ("CONTEXTLATTICE CONTEXT PACK for this subtask (compiled from the goal by /memory/context-pack; "
                "UNVERIFIED until you read the cited source):")
_SEARCH_HEADER = ("CONTEXTLATTICE RECALL for this subtask (seeded from the goal; UNVERIFIED "
                  "until you read the source)")


def _load_config() -> Dict[str, Any]:
    cfg = dict(_DEFAULTS)
    try:
        from hermes_cli.config import load_config_readonly
        block = (load_config_readonly().get("memory") or {}).get("contextlattice") or {}
        if isinstance(block, dict):
            cfg.update({k: v for k, v in block.items() if v not in (None, "")})
    except Exception:
        pass
    if os.environ.get("CONTEXTLATTICE_ORCHESTRATOR_URL"):
        cfg["url"] = os.environ["CONTEXTLATTICE_ORCHESTRATOR_URL"]
    return cfg


def _read_key(env_file: str) -> str:
    """Use Hermes' profile-scoped secret resolver when available; legacy fallback is unscoped."""
    try:
        secret_scope = importlib.import_module("agent.secret_scope")
    except ModuleNotFoundError as exc:
        if exc.name not in {"agent", "agent.secret_scope"}:
            return ""
        secret_scope = None
    except Exception:
        return ""

    try:
        resolver = getattr(secret_scope, "get_secret", None) if secret_scope is not None else None
    except Exception:
        return ""
    if resolver is not None:
        if not callable(resolver):
            return ""
        try:
            value = resolver(_KEY_NAME)
        except Exception:
            return ""
        return str(value).strip() if value else ""

    # Legacy compatibility only for Hermes versions without the scoped resolver.
    # These process-wide sources are not safe for per-profile secrets in multiplex mode.
    legacy_env = os.environ.get(_KEY_NAME, "").strip()
    if legacy_env:
        return legacy_env
    if not env_file:
        env_file = os.environ.get("CONTEXTLATTICE_ENV_FILE", "")
    if not env_file:
        return ""
    try:
        key = ""
        for line in Path(env_file).expanduser().read_text().splitlines():
            if line.startswith(_KEY_NAME + "="):
                key = line.split("=", 1)[1].strip()
        return key
    except Exception:
        return ""


def _slug(text: str, n: int = 6) -> str:
    words = re.findall(r"[a-z0-9]+", text.lower())
    return "-".join(words[:n]) or "entry"


def _mirror_target(content: str) -> Tuple[str, str]:
    """(topic_path, file) for one built-in memory entry. Shared by mirror + reconcile."""
    content = content.strip()
    slug = _slug(content)
    digest = hashlib.sha256(content.encode()).hexdigest()[:10]
    return f"memory/durable/{slug}", f"notes/hermes/memory-mirror/{slug}-{digest}.md"


def _goals_of(args: Dict[str, Any]) -> List[str]:
    tasks = args.get("tasks")
    if isinstance(tasks, list) and tasks:
        return [str((t or {}).get("goal") or "") for t in tasks]
    return [str(args.get("goal") or "")]


def _goals_key(goals: List[str]) -> str:
    return hashlib.sha256(json.dumps(goals, ensure_ascii=False).encode()).hexdigest()


def _text_digest(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def _host_identity(value: Any) -> str:
    return value if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_.:-]{1,180}", value) else ""


def _nonempty_digest(value: Any) -> str:
    if not isinstance(value, str):
        return ""
    digest = value.strip().lower()
    if digest.startswith("sha256:"):
        digest = digest[len("sha256:"):]
    if len(digest) != 64 or any(char not in "0123456789abcdef" for char in digest):
        return ""
    return digest


def _valid_outbox_id(value: Any) -> bool:
    return isinstance(value, str) and bool(value.strip())


class LifecycleNotConfirmed(RuntimeError):
    """The write endpoint did not confirm the requested memory lifecycle."""


def _schema(name, desc, props, required):
    return {"name": name, "description": desc,
            "parameters": {"type": "object", "properties": props, "required": required}}


CL_SEARCH_SCHEMA = _schema(
    "cl_search",
    "Search ContextLattice memory (scoped). Use before planning/diagnosis; broaden once (drop topic_path) if empty.",
    {"query": {"type": "string"},
     "project": {"type": "string", "description": "e.g. hermes-agent, data-plotly, data-tam"},
     "topic_path": {"type": "string", "description": "optional topic prefix, e.g. memory/archive"},
     "mode": {"type": "string", "enum": ["fast", "balanced", "deep"]},
     "limit": {"type": "integer"},
     "drain": {"type": "boolean", "description": "wait (bounded) for slower sources; default true"}},
    ["query"],
)
CL_WRITE_SCHEMA = _schema(
    "cl_write",
    "Write a durable ContextLattice note (decision/outcome/lesson) with verified readback.",
    {"project": {"type": "string"}, "topic_path": {"type": "string"},
     "file": {"type": "string", "description": "notes/<agent>/<name>.md"},
     "content": {"type": "string"}},
    ["project", "topic_path", "content"],
)


class ContextLatticeProvider(MemoryProvider):
    def __init__(self) -> None:
        self._cfg = _load_config()
        self._key = ""
        self._session_id = ""
        self._parent_session_id = ""
        self._primary = True
        self._pending_handoff = False
        self._last_status = "init"
        self._home = Path(os.environ.get("HERMES_HOME") or Path.home() / ".hermes")
        self._threads: List[threading.Thread] = []
        self._lock = threading.Lock()
        self._outbox_lock = threading.Lock()
        self._outbox_drain_lock = threading.Lock()
        self._pending_outcomes: Dict[str, List[List[Dict[str, Any]]]] = {}
        self._pending_invocations: Dict[str, int] = {}
        self._pending_generations: Dict[str, str] = {}
        self._ambiguous_outcomes = set()
        self._invocation_times: Dict[str, float] = {}
        self._closed_calls: Dict[str, float] = {}
        self._session_bindings: Dict[Tuple[str, str], str] = {}
        self._session_binding_times: Dict[Tuple[str, str], float] = {}
        self._session_binding_lock = threading.Lock()

    # -- identity / availability --------------------------------------------------
    @property
    def name(self) -> str:
        return "contextlattice"

    def is_available(self) -> bool:
        return bool(_read_key(self._cfg["env_file"]))

    def initialize(self, session_id: str, **kwargs) -> None:
        self._cfg = _load_config()
        self._key = _read_key(self._cfg["env_file"])
        self._session_id = session_id or ""
        self._primary = kwargs.get("agent_context", "primary") == "primary"
        if kwargs.get("hermes_home"):
            self._home = Path(kwargs["hermes_home"])
        if self._primary and self._key:
            self._bg(self._startup_maintenance, "contextlattice-maintenance")

    def system_prompt_block(self) -> str:
        # STATIC (prompt-cache safe): no counts, no timestamps.
        return (
            "# ContextLattice Memory\n"
            "Active provider. Per-turn scoped recall arrives in <memory-context> (fast mode, UNVERIFIED until "
            "the source is read). Use cl_search for scoped/deeper recall and cl_write for durable notes. "
            "A 'CONTEXTLATTICE DEGRADED' line means recall is incomplete: say so before memory-dependent claims."
        )

    # -- HTTP -----------------------------------------------------------------------
    def _url(self, path: str) -> str:
        return self._cfg["url"].rstrip("/") + path

    def _post(self, path: str, body: Dict[str, Any], timeout: Optional[float] = None) -> Dict[str, Any]:
        req = urllib.request.Request(
            self._url(path), data=json.dumps(body).encode(),
            headers={"Content-Type": "application/json", "X-API-Key": self._key}, method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout or float(self._cfg["timeout"])) as resp:
                return json.loads(resp.read().decode() or "{}")
        except urllib.error.HTTPError as exc:
            exc.close()
            raise

    def _get(self, path: str, timeout: Optional[float] = None) -> Tuple[int, str]:
        req = urllib.request.Request(self._url(path), headers={"X-API-Key": self._key}, method="GET")
        try:
            with urllib.request.urlopen(req, timeout=timeout or float(self._cfg["timeout"])) as resp:
                return resp.status, resp.read().decode()
        except urllib.error.HTTPError as exc:
            try:
                return exc.code, exc.read().decode(errors="replace")
            finally:
                exc.close()

    def _search(self, query: str, *, project: Optional[str] = None, topic_path: Optional[str] = None,
                mode: Optional[str] = None, limit: Optional[int] = None) -> Dict[str, Any]:
        body: Dict[str, Any] = {"query": query[:400], "include_grounding": True,
                                "mode": mode or self._cfg["mode"], "limit": int(limit or self._cfg["limit"])}
        if project:
            body["project"] = project
        if topic_path:
            body["topic_path"] = topic_path
        return self._post("/memory/search", body)

    def _write(self, project: str, topic_path: str, file: str, content: str,
               lifecycle: Optional[str] = None) -> Dict[str, Any]:
        body = {"projectName": project, "topicPath": topic_path,
                "fileName": file, "content": content}
        if lifecycle:
            body["lifecycle"] = lifecycle
        response = self._post("/memory/write", body)
        if response.get("ok") is not True:
            raise RuntimeError("write_unconfirmed")
        if lifecycle and str(response.get("lifecycle") or "") != lifecycle:
            raise LifecycleNotConfirmed("write_lifecycle_unconfirmed")
        return response

    def _file_exists(self, project: str, file: str) -> bool:
        path = "/memory/files/" + urllib.parse.quote(project) + "/" + urllib.parse.quote(file)
        status, _ = self._get(path)
        if status == 200:
            return True
        if status == 404:
            return False
        raise RuntimeError(f"/memory/files HTTP {status}")

    # -- continuation (C) -------------------------------------------------------------
    def _drain(self, cont: Dict[str, Any], budget_s: float) -> Dict[str, Any]:
        """Poll a continuation until it leaves `running` or the budget ends. Returns
        {status, results, failed_sources, result_state}. Never raises."""
        poll = str(cont.get("poll_url") or "")
        if not poll:
            return {"status": "no_poll_url", "results": []}
        path = urllib.parse.urlparse(poll).path if poll.startswith("http") else poll
        deadline = time.monotonic() + max(0.0, budget_s)
        status: str = "running"
        last: Dict[str, Any] = {}
        while True:
            try:
                code, text = self._get(path, timeout=5)
                last = json.loads(text or "{}") if code == 200 else {}
                status = str(last.get("status") or last.get("async_status") or f"http_{code}")
            except Exception as exc:
                status = f"poll_error:{type(exc).__name__}"
            if status != "running" or time.monotonic() >= deadline:
                break
            time.sleep(0.5)
        raw_result = last.get("result")
        result: Dict[str, Any] = raw_result if isinstance(raw_result, dict) else {}
        raw_summary = result.get("source_summary")
        summary: Dict[str, Any] = raw_summary if isinstance(raw_summary, dict) else {}
        return {"status": status, "results": [r for r in (result.get("results") or []) if isinstance(r, dict)],
                "failed_sources": summary.get("failed_sources") or [],
                "pending_sources": summary.get("pending_sources") or [],
                "result_state": result.get("result_state")}

    # -- recall ---------------------------------------------------------------------
    @staticmethod
    def _fmt_hits(label: str, d: Dict[str, Any], seen: set) -> List[str]:
        out = []
        # "had hits" is judged BEFORE dedup: hits already shown under an earlier label are still hits.
        had_hits = any(isinstance(r, dict) for r in (d.get("results") or []))
        for r in (d.get("results") or [])[:8]:
            if not isinstance(r, dict):
                continue
            f = r.get("file") or r.get("topic_path") or "?"
            key = r.get("content_hash") or f
            if key in seen:
                continue
            seen.add(key)
            body = " ".join(str(r.get("summary") or r.get("content") or "").split())[:220]
            try:
                sc = " score=%.3f" % float(r.get("score"))
            except Exception:
                sc = ""
            out.append(f"  - [{r.get('project') or '?'}] {f}{sc}")
            if body:
                out.append(f"      {body}")
        if out:
            out.insert(0, f"{label}:")
        if d.get("degraded") or d.get("error"):
            out.append(f"  ({label} DEGRADED=true - incomplete)")
        cont = d.get("continuation_async") or {}
        if cont:
            pending = cont.get("pending_sources") or []
            status = str(cont.get("status") or cont.get("async_status") or cont.get("result_state") or "").lower()
            waiting = bool(pending) or status in {"queued", "running", "pending", "partial"}
            if had_hits and waiting:
                # Fast sources answered; the continuation only covers slower optional sources.
                out.append(f"  ({label}: fast-source hits are complete; slower sources "
                           f"{pending} still running - cl_search drains them)")
            elif waiting:
                out.append(f"{label}: no fast hits; slower sources {pending} still running - WARMING, "
                           "not absent: run cl_search before concluding absence")
            elif status in {"failed", "degraded", "error"} and not had_hits:
                out.append(f"{label}: continuation failed - DEGRADED, not verified-empty")
        return out

    @staticmethod
    def _continuation_pending(d: Dict[str, Any]) -> bool:
        cont = d.get("continuation_async") or {}
        if not isinstance(cont, dict) or not cont:
            return False
        status = str(cont.get("status") or cont.get("async_status") or cont.get("result_state") or "").lower()
        return bool(cont.get("pending_sources")) or status in {"queued", "running", "pending", "partial"}

    @classmethod
    def _handoff_search_complete(cls, d: Dict[str, Any]) -> bool:
        if d.get("error") or d.get("degraded"):
            return False
        if any(isinstance(row, dict) for row in (d.get("results") or [])):
            return True
        cont = d.get("continuation_async") or {}
        status = str(cont.get("status") or cont.get("async_status") or cont.get("result_state") or "").lower()
        return not cls._continuation_pending(d) and status not in {"failed", "degraded", "error"}

    def prefetch(self, query: str, *, session_id: str = "") -> str:
        if not query or not query.strip() or not self._key:
            return ""
        jobs = {
            "MEMORY ARCHIVE (hermes-agent)": dict(project=self._cfg["project"], topic_path="memory"),
            "CROSS-PROJECT RECALL": dict(),
        }
        if self._pending_handoff:
            jobs["COMPACTION HANDOFF"] = dict(project=self._cfg["project"], topic_path=_HANDOFF_TOPIC,
                                              limit=3)
        results: Dict[str, Any] = {}
        errors: List[str] = []
        with ThreadPoolExecutor(max_workers=len(jobs)) as ex:
            futs = {label: ex.submit(self._search, query, **kw) for label, kw in jobs.items()}
            for label, fut in futs.items():
                try:
                    results[label] = fut.result()
                except Exception as exc:  # network/HTTP: surface, never silent
                    errors.append(f"{label}: {type(exc).__name__}")
        if errors and not results:
            self._last_status = "degraded"
            return ("CONTEXTLATTICE DEGRADED THIS TURN (" + "; ".join(errors) + "). Recall is absent, "
                    "not verified-empty. Use local memory (MEMORY.md) and say degraded before memory-dependent claims.")
        reported_errors = list(errors)
        for label, result in results.items():
            if result.get("error") or result.get("degraded"):
                reported_errors.append(f"{label}: response_degraded")
        self._last_status = "degraded" if reported_errors else "ok"
        if ("COMPACTION HANDOFF" in results
                and self._handoff_search_complete(results["COMPACTION HANDOFF"])):
            self._pending_handoff = False
        seen: set = set()
        lines = ["SEED RECALL (ContextLattice provider, fast mode - UNVERIFIED until you read the source):"]
        for label in ("COMPACTION HANDOFF", "MEMORY ARCHIVE (hermes-agent)", "CROSS-PROJECT RECALL"):
            if label in results:
                lines += self._fmt_hits(label, results[label], seen)
        if reported_errors:
            lines.append("PARTIAL: " + "; ".join(reported_errors))
        if len(lines) == 1:
            lines.append("EMPTY for this prompt (fast seed only) - run cl_search scoped/balanced before concluding absence.")
        return "\n".join(lines)

    # -- tools ----------------------------------------------------------------------
    def get_tool_schemas(self) -> List[Dict[str, Any]]:
        return [CL_SEARCH_SCHEMA, CL_WRITE_SCHEMA]

    def handle_tool_call(self, tool_name: str, args: Dict[str, Any], **kwargs) -> str:
        try:
            if tool_name == "cl_search":
                d = self._search(args["query"], project=args.get("project"), topic_path=args.get("topic_path"),
                                 mode=args.get("mode") or "balanced", limit=args.get("limit") or 8)
                rows = [r for r in (d.get("results") or []) if isinstance(r, dict)]
                cont = d.get("continuation_async") or {}
                cont_report: Optional[Dict[str, Any]] = None
                if cont and args.get("drain", True) is not False:
                    drained = self._drain(cont, float(self._cfg["drain_seconds"]))
                    have = {r.get("content_hash") or r.get("file") for r in rows}
                    added = [r for r in drained["results"] if (r.get("content_hash") or r.get("file")) not in have]
                    rows += added
                    cont_report = {"status": drained["status"], "added_results": len(added),
                                   "failed_sources": drained.get("failed_sources"),
                                   "still_pending": drained.get("pending_sources"),
                                   "result_state": drained.get("result_state")}
                elif cont:
                    cont_report = {"status": "not_drained", "pending_sources": cont.get("pending_sources"),
                                   "poll_url": cont.get("poll_url")}
                hits = [{k: r.get(k) for k in ("project", "file", "topic_path", "score", "summary", "created_at")}
                        for r in rows]
                degraded = bool(d.get("degraded") or d.get("error"))
                return json.dumps({"results": hits, "degraded": degraded,
                                   "status": "degraded" if degraded else "ok",
                                   "continuation": cont_report})
            if tool_name == "cl_write":
                file = args.get("file") or f"notes/hermes/{_slug(args['content'])}.md"
                w = self._write(args["project"], args["topic_path"], file, args["content"])
                rb = self._search(args["content"][:200], project=args["project"],
                                  topic_path=args["topic_path"], limit=5)
                digest = _nonempty_digest(w.get("content_hash"))
                found = bool(digest) and any(
                    self._readback_matches(r, args["project"], file, digest)
                    for r in (rb.get("results") or []) if isinstance(r, dict)
                )
                return json.dumps({"written": True, "file": file, "content_hash": digest or None,
                                   "readback_found": found,
                                   "readback_status": "verified" if found else "unverified"})
        except Exception as exc:
            return json.dumps({"error": type(exc).__name__, "degraded": True})
        return json.dumps({"error": "unknown_tool"})

    @staticmethod
    def _readback_matches(row: Dict[str, Any], project: str, file: str, digest: str) -> bool:
        expected_digest = _nonempty_digest(digest)
        if not expected_digest or _nonempty_digest(row.get("content_hash")) != expected_digest:
            return False
        project_values = [row[name] for name in ("project", "projectName", "project_name")
                          if row.get(name) not in (None, "")]
        file_values = [row[name] for name in ("file", "fileName", "file_name")
                       if row.get(name) not in (None, "")]
        if not project_values or any(str(value).strip() != project for value in project_values):
            return False
        if not file_values or any(str(value).strip() != file for value in file_values):
            return False
        return True

    # -- lifecycle hooks --------------------------------------------------------------
    def _bg(self, fn, name: str) -> None:
        t = spawn_context_thread(fn, name=name)
        with self._lock:
            self._threads = [x for x in self._threads if x.is_alive()] + [t]
        t.start()

    def _state_dir(self) -> Path:
        d = self._home / "contextlattice-plugin"
        d.mkdir(parents=True, exist_ok=True)
        return d

    def _mirror_body(self, action: str, target: str, content: str) -> str:
        prefix = {
            "remove": "RETIRED (removed from built-in memory): ",
            "superseded": "SUPERSEDED (replaced in built-in memory): ",
        }.get(action, "")
        return (f"{prefix}{content.strip()}\n\n[mirror of built-in memory {action} target={target} "
                f"session={self._session_id} at {_dt.datetime.utcnow().isoformat()}Z]")

    def _mirror_item(self, action: str, target: str, content: str, lifecycle: str) -> Dict[str, Any]:
        topic, file = _mirror_target(content)
        return {"project": self._cfg["project"], "topic_path": topic, "file": file,
                "content": self._mirror_body(action, target, content), "lifecycle": lifecycle}

    def on_memory_write(self, action: str, target: str, content: str,
                        metadata: Optional[Dict[str, Any]] = None) -> None:
        if not self._primary or not self._key:
            return
        metadata = metadata if isinstance(metadata, dict) else {}
        previous = metadata.get("previous_content")
        previous = previous.strip() if isinstance(previous, str) else ""
        current = content.strip() if isinstance(content, str) else ""
        action = str(action or "").lower()
        items: List[Dict[str, Any]] = []
        if action == "remove":
            old = previous or current
            if old:
                items.append(self._mirror_item("remove", target, old, "retired"))
            else:
                logger.error("contextlattice removal BLOCKED: previous content is unavailable")
        elif action == "replace":
            if previous and previous != current:
                items.append(self._mirror_item("superseded", target, previous, "superseded"))
            elif not previous:
                logger.error("contextlattice replacement retirement BLOCKED: previous_content is unavailable")
            if current:
                items.append(self._mirror_item("replace", target, current, "durable"))
        elif current:
            items.append(self._mirror_item(action or "write", target, current, "durable"))
        if not items:
            return

        def _run():
            for item in items:
                try:
                    self._write(item["project"], item["topic_path"], item["file"],
                                item["content"], lifecycle=item["lifecycle"])
                except LifecycleNotConfirmed:
                    try:
                        self._outbox_append(item)
                        logger.error("contextlattice lifecycle update BLOCKED by write endpoint; queued for retry")
                    except Exception as queue_exc:
                        logger.error("contextlattice lifecycle update BLOCKED; durable outbox unavailable (%s)",
                                     type(queue_exc).__name__)
                except Exception as exc:
                    try:
                        self._outbox_append(item)
                        logger.warning("contextlattice mirror failed; queued for retry (%s)", type(exc).__name__)
                    except Exception as queue_exc:
                        logger.error("contextlattice mirror BLOCKED; durable outbox unavailable (%s)",
                                     type(queue_exc).__name__)
        self._bg(_run, "contextlattice-mirror")

    # -- outbox + reconciliation (B) --------------------------------------------------
    def _outbox_path(self) -> Path:
        return self._state_dir() / "mirror-outbox.jsonl"

    @contextlib.contextmanager
    def _outbox_process_lock(self, name: str):
        if fcntl is None:
            raise RuntimeError("process_safe_outbox_lock_unavailable")
        path = self._state_dir() / name
        with path.open("a+") as lock_file:
            fcntl.flock(lock_file.fileno(), fcntl.LOCK_EX)
            try:
                yield
            finally:
                fcntl.flock(lock_file.fileno(), fcntl.LOCK_UN)

    @staticmethod
    def _serialize_outbox(items: List[Dict[str, Any]]) -> bytes:
        if len(items) > _OUTBOX_MAX_ITEMS:
            raise RuntimeError("outbox_item_limit_exceeded")
        rows = []
        for item in items:
            row = json.dumps(item, sort_keys=True, separators=(",", ":")).encode()
            if len(row) > _OUTBOX_MAX_ITEM_BYTES:
                raise RuntimeError("outbox_item_size_exceeded")
            rows.append(row + b"\n")
        payload = b"".join(rows)
        if len(payload) > _OUTBOX_MAX_BYTES:
            raise RuntimeError("outbox_size_limit_exceeded")
        return payload

    @staticmethod
    def _read_outbox(path: Path) -> List[Dict[str, Any]]:
        if not path.exists():
            return []
        raw = path.read_bytes()
        if len(raw) > _OUTBOX_MAX_BYTES:
            raise RuntimeError("outbox_size_limit_exceeded")
        items = []
        for index, line in enumerate(raw.splitlines()):
            if not line.strip():
                continue
            if len(line) > _OUTBOX_MAX_ITEM_BYTES:
                raise RuntimeError("outbox_item_size_exceeded")
            try:
                item = json.loads(line)
            except Exception:
                raise RuntimeError("outbox_record_invalid") from None
            if not isinstance(item, dict):
                raise RuntimeError("outbox_record_invalid")
            if not _valid_outbox_id(item.get("_outbox_id")):
                item["_outbox_id"] = "legacy-" + hashlib.sha256(
                    str(index).encode() + b":" + line
                ).hexdigest()
            items.append(item)
            if len(items) > _OUTBOX_MAX_ITEMS:
                raise RuntimeError("outbox_item_limit_exceeded")
        return items

    @classmethod
    def _atomic_write_outbox(cls, path: Path, items: List[Dict[str, Any]]) -> None:
        payload = cls._serialize_outbox(items)
        if not payload:
            try:
                path.unlink()
            except FileNotFoundError:
                pass
            return
        fd, temporary = tempfile.mkstemp(prefix=".mirror-outbox-", dir=str(path.parent))
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(payload)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, path)
            temporary = ""
        finally:
            if temporary and os.path.exists(temporary):
                os.unlink(temporary)

    def _outbox_append(self, item: Dict[str, Any]) -> None:
        path = self._outbox_path()
        with self._outbox_lock, self._outbox_process_lock("mirror-outbox.lock"):
            items = self._read_outbox(path)
            queued = dict(item)
            queued["_outbox_id"] = uuid.uuid4().hex
            items.append(queued)
            self._atomic_write_outbox(path, items)

    def drain_outbox(self) -> Dict[str, int]:
        path = self._outbox_path()
        with self._outbox_drain_lock, self._outbox_process_lock("mirror-outbox-drain.lock"):
            with self._outbox_lock, self._outbox_process_lock("mirror-outbox.lock"):
                items = self._read_outbox(path)
            if not items:
                return {"sent": 0, "kept": 0, "blocked": 0}
            sent_ids = set()
            sent = 0
            blocked = 0
            for it in items[:_OUTBOX_DRAIN_BATCH]:
                try:
                    self._write(it["project"], it["topic_path"], it["file"], it["content"],
                                lifecycle=it.get("lifecycle"))
                    sent_ids.add(it["_outbox_id"])
                    sent += 1
                except LifecycleNotConfirmed:
                    blocked += 1
                    logger.error("contextlattice lifecycle update BLOCKED; outbox item retained")
                except Exception as exc:
                    logger.warning("contextlattice outbox retry failed; item retained (%s)",
                                   type(exc).__name__)
            with self._outbox_lock, self._outbox_process_lock("mirror-outbox.lock"):
                current = self._read_outbox(path)
                remaining = [it for it in current if it.get("_outbox_id") not in sent_ids]
                self._atomic_write_outbox(path, remaining)
            return {"sent": sent, "kept": len(remaining), "blocked": blocked}

    def reconcile(self, repair: bool = True) -> Dict[str, Any]:
        """Diff every built-in MEMORY.md / USER.md entry against its CL mirror file;
        backfill missing ones. Present-check is by exact file (content digest in the name)."""
        report: Dict[str, Any] = {"checked": 0, "present": 0, "missing": [], "repaired": 0, "errors": []}
        project = self._cfg["project"]
        for target, fname in (("memory", "MEMORY.md"), ("user", "USER.md")):
            p = self._home / "memories" / fname
            if not p.exists():
                continue
            for entry in (e.strip() for e in p.read_text().split(_ENTRY_DELIMITER)):
                if not entry:
                    continue
                report["checked"] += 1
                topic, file = _mirror_target(entry)
                try:
                    if self._file_exists(project, file):
                        report["present"] += 1
                        continue
                    report["missing"].append(file)
                    if repair:
                        self._write(project, topic, file, self._mirror_body("reconcile", target, entry))
                        report["repaired"] += 1
                except Exception as exc:
                    report["errors"].append(f"{file}: {type(exc).__name__}")
        if repair and not report["errors"]:
            (self._state_dir() / "reconcile.json").write_text(
                json.dumps({"at": time.time(), **{k: report[k] for k in ("checked", "present", "repaired")}}))
        return report

    def _reconcile_due(self) -> bool:
        stamp = self._state_dir() / "reconcile.json"
        try:
            last = float(json.loads(stamp.read_text()).get("at") or 0)
        except Exception:
            return True
        return time.time() - last >= float(self._cfg["reconcile_hours"]) * 3600

    def _startup_maintenance(self) -> None:
        try:
            outbox = self.drain_outbox()
            if outbox["blocked"]:
                logger.error("contextlattice startup outbox blocked=%s kept=%s",
                             outbox["blocked"], outbox["kept"])
            if self._reconcile_due():
                r = self.reconcile(repair=True)
                logger.info("contextlattice reconcile: checked=%s present=%s repaired=%s errors=%s",
                            r["checked"], r["present"], r["repaired"], len(r["errors"]))
        except Exception as exc:
            logger.error("contextlattice startup maintenance BLOCKED (%s)", type(exc).__name__)

    def _cmd_reconcile(self, raw_args: str = "") -> str:
        if not self._key:
            return "ContextLattice reconcile: no API key (provider inactive)."
        dry = "--dry-run" in (raw_args or "").split()
        if dry:
            o = {"sent": 0, "kept": 0, "blocked": 0}
        else:
            try:
                o = self.drain_outbox()
            except Exception as exc:
                return (f"ContextLattice reconcile (repair): outbox BLOCKED ({type(exc).__name__}); "
                        "reconciliation was not attempted.")
        r = self.reconcile(repair=not dry)
        outbox = ("outbox unchanged" if dry else
                  f"outbox sent={o['sent']} kept={o['kept']} blocked={o['blocked']}")
        return (f"ContextLattice reconcile ({'dry-run' if dry else 'repair'}): {outbox}; "
                f"entries checked={r['checked']} present={r['present']} missing={len(r['missing'])} "
                f"repaired={r['repaired']} errors={len(r['errors'])}")

    def on_pre_compress(self, messages: List[Dict[str, Any]], **kwargs) -> str:
        if not self._primary or not self._key:
            return ""
        users = [str(m.get("content") or "") for m in messages if m.get("role") == "user"]
        users = [u for u in users if u.strip() and "CONTEXT COMPACTION" not in u][-5:]
        last_asst = next((str(m.get("content") or "") for m in reversed(messages)
                          if m.get("role") == "assistant" and m.get("content")), "")
        content = (
            f"Pre-compaction handoff (contextlattice provider) session={self._session_id} "
            f"at {_dt.datetime.utcnow().isoformat()}Z, {len(messages)} messages compacted.\n\n"
            "## Last user messages (verbatim, newest last)\n" + "\n".join(f"> {u[:600]}" for u in users)
            + "\n\n## Last assistant output\n" + last_asst[:1500]
        )
        file = f"notes/hermes/handoff/{self._session_id or 'session'}.md"
        try:
            w = self._write(self._cfg["project"], _HANDOFF_TOPIC, file, content)
            digest = _nonempty_digest(w.get("content_hash"))
            receipt = f"content hash={digest[:12]}" if digest else "content hash unavailable"
            return f"ContextLattice handoff checkpoint write accepted; {receipt}."
        except Exception as exc:
            logger.warning("contextlattice pre-compress handoff failed (%s)", type(exc).__name__)
            return "ContextLattice handoff checkpoint FAILED (CL unreachable): state is only in this summary."

    def on_session_switch(self, new_session_id: str, *, parent_session_id: str = "",
                          reset: bool = False, **kwargs) -> None:
        self._parent_session_id = parent_session_id or self._session_id
        self._session_id = new_session_id
        if not reset:  # compression/resume/branch: continuation of the same work
            self._pending_handoff = True

    # -- subagent context (A) ------------------------------------------------------------
    def _delegation_recall(self, goal: str) -> str:
        """Fallback: scoped, read-only search recall for one delegated child."""
        project = str(self._cfg["project"])
        try:
            d = self._search(goal, project=project, limit=int(self._cfg.get("delegation_limit") or 4))
        except Exception as exc:
            return f"CONTEXTLATTICE DEGRADED for this subtask ({type(exc).__name__}); no recall injected."
        if d.get("error") or d.get("degraded"):
            return "CONTEXTLATTICE DEGRADED for this subtask; project-scoped fallback is incomplete."
        rows = [r for r in (d.get("results") or []) if isinstance(r, dict)]
        if any(str(r.get("project") or "") != project for r in rows):
            logger.error("contextlattice delegation fallback BLOCKED: project scope mismatch")
            return "CONTEXTLATTICE BLOCKED for this subtask: project scope could not be verified."
        response_project = str(d.get("project") or "")
        if response_project and response_project != project:
            logger.error("contextlattice delegation fallback BLOCKED: response project mismatch")
            return "CONTEXTLATTICE BLOCKED for this subtask: project scope could not be verified."
        scoped = dict(d)
        scoped["results"] = rows
        block = "\n".join(self._fmt_hits(_SEARCH_HEADER, scoped, set()))
        if not block and not self._continuation_pending(scoped) and not scoped.get("error"):
            return "CONTEXTLATTICE RECALL: no verified results in the configured project."
        return block

    def _delegation_pack(self, goal: str, task_id: str, session_id: str = "") -> Tuple[str, Optional[str]]:
        """CL-native context pack for one child: (block, quality sample_id). Falls back to search."""
        if not goal.strip():
            return "", None
        project = str(self._cfg["project"])
        try:
            payload = {
                "query": goal[:400], "mode": "fast",
                "project": project,
                "limit": int(self._cfg.get("delegation_limit") or 6),
                "target_context_pack_tokens": int(self._cfg.get("delegation_tokens") or 1200),
                "task_id": task_id, "task_class": "hermes.delegate_task",
                "agent_id": "hermes_agent",
            }
            if session_id:
                payload["session_id"] = session_id
            d = self._post("/memory/context-pack", payload,
                           timeout=min(max(float(self._cfg["timeout"]), 1.0), 10.0))
            prompt = str(d.get("reference_prompt") or "").strip()
            context_pack = d.get("context_pack")
            pack_project = context_pack.get("project") if isinstance(context_pack, dict) else None
            sample = (d.get("context_pack_quality") or {}).get("sample_id")
            if prompt and str(pack_project or "") == project:
                cap = int(self._cfg.get("delegation_max_chars") or 6000)
                return f"{_PACK_HEADER}\n{prompt[:cap]}", (str(sample) if sample else None)
            if prompt:
                logger.warning("contextlattice context-pack scope unconfirmed; rejecting and using scoped search")
        except Exception as exc:
            logger.info("contextlattice context-pack failed (%s); falling back to scoped search",
                        type(exc).__name__)
        return self._delegation_recall(goal), None

    def _delegation_key(self, goals: List[str], session_id: str, tool_call_id: str) -> str:
        identity = [str(self._cfg["project"]), session_id, tool_call_id]
        if tool_call_id and session_id:
            return "call:" + _text_digest(json.dumps(identity))
        return "legacy:" + _text_digest(json.dumps(identity[:2] + [_goals_key(goals)]))

    def _delegation_session(self, project: str, native_session_id: str) -> str:
        """Reuse an authenticated gateway session, not a host id masquerading as one."""
        if not native_session_id:
            return ""
        with self._session_binding_lock:
            key = (project, native_session_id)
            if key in self._session_bindings and time.monotonic() - self._session_binding_times.get(key, 0) < 60:
                return self._session_bindings[key]
            try:
                response = self._post("/v1/agents/sessions/start", {
                    "project": project, "agent": "hermes-agent", "agent_id": "hermes_agent",
                    "native_session_id": native_session_id, "ensure": True,
                    "objective": "Native Hermes context delivery observations",
                }, timeout=min(float(self._cfg["timeout"]), 2.0))
                row = response.get("session") or {}
                session_id = _host_identity(row.get("id"))
                if (response.get("ok") is not True or not session_id
                        or row.get("project") != project or row.get("agent_id") != "hermes_agent"
                        or row.get("native_session_id") != native_session_id):
                    raise ValueError("session_binding_unconfirmed")
                if len(self._session_bindings) >= 32:
                    oldest = next(iter(self._session_bindings))
                    self._session_bindings.pop(oldest)
                    self._session_binding_times.pop(oldest, None)
                self._session_bindings[key] = session_id
                self._session_binding_times[key] = time.monotonic()
                return session_id
            except Exception as exc:
                logger.warning("contextlattice session observation unavailable (%s)", type(exc).__name__)
                return ""

    def _expire_delegations_locked(self) -> None:
        cutoff = time.monotonic() - _DELEGATION_TTL
        for key, started in list(self._invocation_times.items()):
            if started < cutoff:
                self._pending_outcomes.pop(key, None)
                self._pending_invocations.pop(key, None)
                self._pending_generations.pop(key, None)
                self._ambiguous_outcomes.discard(key)
                self._invocation_times.pop(key, None)
                if key.startswith("call:"):
                    self._closed_calls[key] = time.monotonic()
                logger.warning("contextlattice delegation observation expired; no outcome inferred")
        self._closed_calls = {k: at for k, at in self._closed_calls.items() if at >= cutoff}
        while len(self._closed_calls) > _DELEGATION_MAX_PENDING:
            self._closed_calls.pop(next(iter(self._closed_calls)))

    def _on_pre_tool_call(self, tool_name: str = "", args: Optional[Dict[str, Any]] = None,
                          session_id: str = "", tool_call_id: str = "", **_: Any):
        """Children run with skip_memory=True, so they get no prefetch. Inject one context
        pack per task into its `context`. Never raises (pre_tool_call fails closed)."""
        key = None
        reserved = False
        samples_recorded = False
        generation = ""
        try:
            if tool_name != "delegate_task" or not self._primary or not self._key or not isinstance(args, dict):
                return None
            if self._cfg.get("delegation_recall") is False:
                return None
            goals = _goals_of(args)
            if len(goals) > _DELEGATION_MAX_TASKS:
                logger.warning("contextlattice delegation batch exceeds bounded recall; no injection")
                return None
            native_id = _host_identity(session_id) or _host_identity(self._session_id)
            call_id = _host_identity(tool_call_id)
            key = self._delegation_key(goals, native_id, call_id)
            with self._lock:
                self._expire_delegations_locked()
                if key in self._closed_calls or sum(self._pending_invocations.values()) >= _DELEGATION_MAX_PENDING:
                    logger.warning("contextlattice duplicate or capacity-limited delegation; no injection")
                    return None
                outstanding = self._pending_invocations.get(key, 0)
                if outstanding == 0:
                    self._pending_generations[key] = uuid.uuid4().hex
                generation = self._pending_generations[key]
                self._pending_invocations[key] = outstanding + 1
                self._invocation_times.setdefault(key, time.monotonic())
                ambiguous = outstanding > 0 and key not in self._ambiguous_outcomes
                if outstanding > 0:
                    self._ambiguous_outcomes.add(key)
                reserved = True
            if ambiguous:
                logger.warning("contextlattice outcomes abstained for concurrent identical delegations")
            invocation_id = uuid.uuid4().hex
            ids = [f"hermes-{invocation_id}-{i}" for i in range(len(goals))]
            project = str(self._cfg["project"])
            gateway_session = self._delegation_session(project, native_id)
            with ThreadPoolExecutor(max_workers=min(8, len(goals))) as ex:
                packs = list(ex.map(lambda pair: self._delegation_pack(pair[0], pair[1], gateway_session),
                                    zip(goals, ids)))
            tasks = args.get("tasks")
            contexts = []
            if isinstance(tasks, list) and tasks:
                new = []
                for t, (block, _s) in zip(tasks, packs):
                    t = dict(t or {})
                    if block:
                        t["context"] = ((t.get("context") or "") + "\n\n" + block).strip()
                    contexts.append(str(t.get("context") or ""))
                    new.append(t)
                directive = {"action": "modify", "args": {"tasks": new}}
            else:
                block = packs[0][0] if packs else ""
                contexts = [((args.get("context") or "") + "\n\n" + block).strip()]
                directive = {"action": "modify", "args": {"context": contexts[0]}} if block else None
            records = [{"project": project, "session_id": gateway_session, "task_id": ids[i],
                        "sample_id": sample, "invocation_id": invocation_id, "task_index": i,
                        "native_session_digest": _text_digest(native_id),
                        "tool_call_digest": _text_digest(call_id) if call_id else "",
                        "goals_digest": _goals_key(goals), "rendered_digest": _text_digest(block),
                        "rendered_bytes": len(block.encode()), "context_digest": _text_digest(contexts[i]),
                        "context_bytes": len(contexts[i].encode()),
                        "correlation": "host_tool_call_id" if key.startswith("call:") else "legacy_goals"}
                       for i, (block, sample) in enumerate(packs)]
            with self._lock:
                if self._pending_generations.get(key) != generation:
                    return None  # Host already cancelled/timed out this callback.
                self._pending_outcomes.setdefault(key, []).append(records)
            samples_recorded = True
            return directive
        except Exception as exc:
            if reserved and not samples_recorded:
                with self._lock:
                    if self._pending_generations.get(key) == generation:
                        self._pending_outcomes.setdefault(key, []).append([])
            logger.warning("contextlattice delegation context failed (%s)", type(exc).__name__)
        return None

    def _on_post_tool_call(self, tool_name: str = "", args: Optional[Dict[str, Any]] = None,
                           result: Any = None, session_id: str = "", tool_call_id: str = "",
                           **_: Any) -> None:
        """Observe host results. Completion is neither first-pass success nor verification."""
        try:
            if tool_name != "delegate_task" or not isinstance(args, dict):
                return None
            goals = _goals_of(args)
            native_id = _host_identity(session_id) or _host_identity(self._session_id)
            key = self._delegation_key(goals, native_id, _host_identity(tool_call_id))
            with self._lock:
                self._expire_delegations_locked()
                outstanding = self._pending_invocations.get(key, 0)
                if not outstanding:
                    return None
                ambiguous = key in self._ambiguous_outcomes
                queues = self._pending_outcomes.get(key)
                samples = queues.pop(0) if queues else None
                if queues == []:
                    self._pending_outcomes.pop(key, None)
                if outstanding == 1:
                    self._pending_invocations.pop(key, None)
                    self._pending_generations.pop(key, None)
                    self._invocation_times.pop(key, None)
                    self._ambiguous_outcomes.discard(key)
                    if key.startswith("call:"):
                        self._closed_calls[key] = time.monotonic()
                else:
                    self._pending_invocations[key] = outstanding - 1
            if ambiguous:
                return None
            if not samples or any(r["goals_digest"] != _goals_key(goals) for r in samples):
                return None
            entries: Dict[int, Dict[str, Any]] = {}
            try:
                text = result if isinstance(result, str) else json.dumps(result)
                if len(text.encode()) > 256 * 1024:
                    raise ValueError("result_too_large")
                parsed = json.loads(text[text.index("{"):]) if "{" in text else {}
                rows = parsed.get("results") or []
                if not isinstance(rows, list) or len(rows) > _DELEGATION_MAX_TASKS:
                    raise ValueError("invalid_results")
                for e in rows:
                    if (not isinstance(e, dict) or type(e.get("task_index")) is not int
                            or not 0 <= e["task_index"] < len(samples) or e["task_index"] in entries):
                        raise ValueError("ambiguous_result_index")
                    entries[e["task_index"]] = e
            except Exception:
                entries = {}
            observed_tasks = args.get("tasks")
            contexts = ([str((t or {}).get("context") or "") for t in observed_tasks]
                        if isinstance(observed_tasks, list) and observed_tasks else [str(args.get("context") or "")])
            for idx, record in enumerate(samples):
                e = entries.get(idx) or {}
                status = e.get("status")
                status = status if isinstance(status, str) and status in {
                    "completed", "failed", "interrupted", "cancelled", "error"} else "unknown"
                # Hermes middleware can dispatch replacement arguments while
                # post hooks echo earlier ones. Neither echo nor status proves
                # which task/context actually reached the child.
                observation = dict(record, host_status="unknown", host_reported_status=status,
                                   post_hook_context_match=(_text_digest(contexts[idx]) == record["context_digest"]),
                                   first_pass_success_known=False, retry_count_known=False,
                                   dispatch_observed=False, delivery_confirmed=False,
                                   consumption_observed=False, independent_verification=False,
                                   calibration_eligible=False)
                for field in ("schema_valid", "truncated"):
                    if type(e.get(field)) is bool:
                        observation[field] = e[field]
                self._bg(lambda r=observation: self._post_observation(r), "contextlattice-observation")
        except Exception as exc:
            logger.warning("contextlattice delegation outcome failed (%s)", type(exc).__name__)
        return None

    def _post_observation(self, observation: Dict[str, Any]) -> None:
        session_id = observation["session_id"]
        if not session_id:
            logger.warning("contextlattice delegation observation unrecorded: session binding unavailable")
            return
        try:
            response = self._post("/v1/agents/sessions/" + session_id + "/events", {
                "type": "context.delivery.observed", "agent": "hermes-agent", "agent_id": "hermes_agent",
                "project": observation["project"], "summary": "Native delegation observation; not verified utility",
                "metadata": observation,
            })
            if response.get("ok") is not True:
                raise ValueError("observation_not_acknowledged")
        except Exception as exc:
            with self._session_binding_lock:
                for key, value in list(self._session_bindings.items()):
                    if value == session_id:
                        self._session_bindings.pop(key, None)
                        self._session_binding_times.pop(key, None)
            logger.warning("contextlattice delegation observation unrecorded (%s)", type(exc).__name__)

    def shutdown(self) -> None:
        with self._lock:
            threads = list(self._threads)
        for t in threads:
            t.join(timeout=3)


def register(ctx) -> None:
    provider = ContextLatticeProvider()
    ctx.register_memory_provider(provider)
    ctx.register_hook("pre_tool_call", provider._on_pre_tool_call)
    ctx.register_hook("post_tool_call", provider._on_post_tool_call)
    try:
        ctx.register_command("cl-reconcile", provider._cmd_reconcile,
                             description="Diff built-in memory vs ContextLattice mirrors; backfill gaps",
                             args_hint="[--dry-run]")
    except Exception as exc:  # command surface is optional; the provider must still load
        logger.info("contextlattice: /cl-reconcile not registered (%s)", type(exc).__name__)
