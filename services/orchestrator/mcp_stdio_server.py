#!/usr/bin/env python3
"""
Minimal stdio MCP bridge for ContextLattice orchestrator.

Designed for managed runners that force an outer `mcp-proxy` wrapper.
This process speaks MCP over stdio and forwards tool calls to the local
orchestrator HTTP service.
"""

from __future__ import annotations

import atexit
import json
import os
import signal
import subprocess
import sys
import time
from dataclasses import dataclass
from typing import Any
from urllib import error as urlerror
from urllib import request as urlrequest


JSONRPC_VERSION = "2.0"
DEFAULT_PROTOCOL_VERSION = "2024-11-05"
SERVER_NAME = "contextlattice-stdio-bridge"
SERVER_VERSION = "1.0.0"
WIRE_MODE = "content-length"


@dataclass
class BridgeConfig:
    orchestrator_host: str
    orchestrator_port: int
    orchestrator_base_url: str
    orchestrator_api_key: str
    start_internal_orchestrator: bool
    startup_timeout_secs: float


def _load_config() -> BridgeConfig:
    host = os.getenv("ORCH_HOST", "127.0.0.1").strip() or "127.0.0.1"
    port = int(os.getenv("ORCH_PORT", "8075"))
    base = os.getenv("ORCH_BASE_URL", f"http://{host}:{port}").strip() or f"http://{host}:{port}"
    api_key = os.getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "").strip()
    start_internal = os.getenv("ORCH_START_INTERNAL", "true").lower() in ("1", "true", "yes", "on")
    startup_timeout = max(5.0, float(os.getenv("ORCH_STARTUP_TIMEOUT_SECS", "30")))
    return BridgeConfig(
        orchestrator_host=host,
        orchestrator_port=port,
        orchestrator_base_url=base.rstrip("/"),
        orchestrator_api_key=api_key,
        start_internal_orchestrator=start_internal,
        startup_timeout_secs=startup_timeout,
    )


TOOLS: list[dict[str, Any]] = [
    {
        "name": "health",
        "description": (
            "Run a non-destructive runtime health check before any memory tool call. "
            "Use this when a connection fails, startup seems incomplete, or you need readiness evidence before writes. "
            "Returns a JSON health envelope (for example: status/services/components/queue fields) as both text and structured JSON. "
            "If the orchestrator requires an API key and the bridge is not configured, this returns an auth failure instead of mutating state."
        ),
        "annotations": {
            "readOnlyHint": True,
            "destructiveHint": False,
            "idempotentHint": True,
            "openWorldHint": False,
        },
        "inputSchema": {
            "type": "object",
            "properties": {},
            "required": [],
            "additionalProperties": False,
        },
        "outputSchema": {
            "type": "object",
            "description": "Health payload from GET /health.",
            "properties": {
                "status": {"type": "string"},
                "services": {"type": "object", "additionalProperties": True},
                "components": {"type": "object", "additionalProperties": True},
                "queue": {"type": "object", "additionalProperties": True},
                "ok": {"type": "boolean"},
            },
            "additionalProperties": True,
        },
    },
    {
        "name": "memory.search",
        "description": (
            "Retrieve contextual memory for the current task before inference. "
            "Required inputs are project + query; add topic_path when you know the scope to reduce noise and latency. "
            "Set include_grounding=true to receive factual grounding blocks (including verbatim numeric copies) and set "
            "include_retrieval_debug=true when diagnosing source timeouts/degraded lanes. "
            "This tool is read-only and returns a lifecycle state (ready/pending/degraded/empty) plus per-source status; "
            "it never writes memory. If the request is unauthorized or upstream is unavailable, isError is true and the error "
            "payload is returned in both text and structured JSON."
        ),
        "annotations": {
            "readOnlyHint": True,
            "destructiveHint": False,
            "idempotentHint": True,
            "openWorldHint": False,
        },
        "inputSchema": {
            "type": "object",
            "properties": {
                "project": {
                    "type": "string",
                    "minLength": 1,
                    "description": "Project identifier to scope retrieval (for example: contextlattice, algotraderv2_rust). Unknown projects can return project_suggestions.",
                },
                "query": {
                    "type": "string",
                    "minLength": 1,
                    "description": "Natural-language retrieval query describing what context is needed now. Keep it specific to improve ranking and reduce continuation work.",
                },
                "topic_path": {
                    "type": "string",
                    "description": "Optional topic hierarchy for scoped retrieval (for example: runbooks/release). Omit for broader recall when scoped reads return empty/degraded.",
                },
                "include_grounding": {
                    "type": "boolean",
                    "description": "When true, response includes a grounding object with factual snippets and strict numeric copies for citation-safe reasoning.",
                    "default": False,
                },
                "include_retrieval_debug": {
                    "type": "boolean",
                    "description": "When true, response includes retrieval debug details (source policy, timings, staged continuation, failures/timeouts).",
                    "default": False,
                },
                "agent_id": {
                    "type": "string",
                    "description": "Optional stable agent identity used to apply retrieval profile defaults (mode/sources/escalation/query expansion).",
                },
            },
            "required": ["project", "query"],
            "additionalProperties": False,
        },
        "outputSchema": {
            "type": "object",
            "description": "Memory search payload from POST /memory/search.",
            "properties": {
                "results": {"type": "array", "items": {"type": "object", "additionalProperties": True}},
                "result_state": {
                    "type": "string",
                    "enum": ["ready", "pending", "degraded", "empty"],
                },
                "degraded": {"type": "boolean"},
                "warnings": {"type": "array", "items": {"type": "string"}},
                "source_summary": {"type": "object", "additionalProperties": True},
                "source_status": {"type": "object", "additionalProperties": True},
                "retrieval_lifecycle": {"type": "object", "additionalProperties": True},
                "grounding": {"type": "object", "additionalProperties": True},
                "retrieval": {"type": "object", "additionalProperties": True},
            },
            "additionalProperties": True,
        },
    },
    {
        "name": "memory.write",
        "description": (
            "Persist a contextual memory item into ContextLattice for future recall. "
            "This is a state-changing operation: it writes to durable memory and may trigger fanout/indexing/rollup updates. "
            "Use for checkpoints, implementation notes, and decisions that should be retrievable later. "
            "If topicPath is omitted, the service derives scope from fileName to keep retrieval grouping stable. "
            "Success is indicated by ok=true and event_id; fanout targets may still be pending/retrying and are reported explicitly. "
            "For retrieval use memory.search; for readiness checks use health."
        ),
        "annotations": {
            "readOnlyHint": False,
            "destructiveHint": False,
            "idempotentHint": False,
            "openWorldHint": False,
        },
        "inputSchema": {
            "type": "object",
            "properties": {
                "projectName": {
                    "type": "string",
                    "minLength": 1,
                    "description": "Project identifier for the write (must match intended retrieval scope and future search project).",
                },
                "fileName": {
                    "type": "string",
                    "minLength": 1,
                    "description": "Logical memory filename/path used for grouping and lookup (for example: notes/codex/xyz.md). Keep stable across updates to preserve continuity.",
                },
                "content": {
                    "type": "string",
                    "minLength": 1,
                    "description": "Memory payload to persist. Keep numeric facts verbatim. Secret handling follows server policy (redact/block/allow).",
                },
                "topicPath": {
                    "type": "string",
                    "description": "Optional topic hierarchy for retrieval scoping (for example: runbooks/runtime-hardening). If omitted, topic is derived from fileName.",
                },
            },
            "required": ["projectName", "fileName", "content"],
            "additionalProperties": False,
        },
        "outputSchema": {
            "type": "object",
            "description": "Memory write acknowledgement payload from POST /memory/write.",
            "properties": {
                "ok": {"type": "boolean"},
                "event_id": {"type": "string"},
                "warnings": {"type": "array", "items": {"type": "string"}},
                "fanout": {"type": "object", "additionalProperties": {"type": "string"}},
                "deduped": {"type": "boolean"},
                "latest_hash_unchanged": {"type": "boolean"},
            },
            "additionalProperties": True,
        },
    },
]


def _stderr(msg: str) -> None:
    print(msg, file=sys.stderr, flush=True)


def _read_message() -> dict[str, Any] | None:
    global WIRE_MODE
    headers: dict[str, str] = {}
    while True:
        line = sys.stdin.buffer.readline()
        if not line:
            return None
        if line in (b"\r\n", b"\n"):
            break
        text = line.decode("utf-8", errors="replace").strip()
        if text.startswith("{"):
            # Some managed MCP proxies send newline-delimited JSON-RPC rather than
            # Content-Length framed messages. Accept that variant fail-open.
            try:
                WIRE_MODE = "jsonl"
                return json.loads(text)
            except json.JSONDecodeError as exc:
                _stderr(f"invalid jsonl request: {exc}")
                continue
        if ":" not in text:
            continue
        key, value = text.split(":", 1)
        headers[key.strip().lower()] = value.strip()
    length = int(headers.get("content-length", "0"))
    if length <= 0:
        return None
    body = sys.stdin.buffer.read(length)
    if not body:
        return None
    return json.loads(body.decode("utf-8"))


def _write_message(payload: dict[str, Any]) -> None:
    data = json.dumps(payload, separators=(",", ":"), ensure_ascii=True).encode("utf-8")
    if WIRE_MODE == "jsonl":
        sys.stdout.buffer.write(data + b"\n")
        sys.stdout.buffer.flush()
        return
    sys.stdout.buffer.write(f"Content-Length: {len(data)}\r\n\r\n".encode("ascii"))
    sys.stdout.buffer.write(data)
    sys.stdout.buffer.flush()


def _tool_result(ok: bool, payload: Any) -> dict[str, Any]:
    text = json.dumps(payload, ensure_ascii=True)
    result: dict[str, Any] = {
        "isError": not ok,
        "content": [{"type": "text", "text": text}],
    }
    if isinstance(payload, dict):
        result["structuredContent"] = payload
    else:
        result["structuredContent"] = {"value": payload}
    return result


def _jsonrpc_result(req_id: Any, result: dict[str, Any]) -> dict[str, Any]:
    return {"jsonrpc": JSONRPC_VERSION, "id": req_id, "result": result}


def _jsonrpc_error(req_id: Any, code: int, message: str) -> dict[str, Any]:
    return {"jsonrpc": JSONRPC_VERSION, "id": req_id, "error": {"code": code, "message": message}}


def _http_json(config: BridgeConfig, method: str, path: str, payload: dict[str, Any] | None = None) -> tuple[int, Any]:
    url = f"{config.orchestrator_base_url}{path}"
    data = None
    headers = {"Content-Type": "application/json"}
    if config.orchestrator_api_key:
        headers["x-api-key"] = config.orchestrator_api_key
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
    req = urlrequest.Request(url=url, data=data, headers=headers, method=method)
    try:
        with urlrequest.urlopen(req, timeout=15) as resp:
            status = int(resp.status)
            body = resp.read().decode("utf-8", errors="replace")
            try:
                return status, json.loads(body) if body else {}
            except json.JSONDecodeError:
                return status, {"raw": body}
    except urlerror.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")
        try:
            parsed = json.loads(body) if body else {"error": str(exc)}
        except json.JSONDecodeError:
            parsed = {"error": body or str(exc)}
        return int(exc.code), parsed
    except Exception as exc:  # pragma: no cover
        return 599, {"error": str(exc)}


class BridgeRuntime:
    def __init__(self, config: BridgeConfig):
        self.config = config
        self.orchestrator_proc: subprocess.Popen[str] | None = None

    def stop(self) -> None:
        proc = self.orchestrator_proc
        if proc is None:
            return
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=3)
            except Exception:
                proc.kill()
        self.orchestrator_proc = None

    def ensure_orchestrator(self) -> None:
        if not self.config.start_internal_orchestrator:
            return
        if self.orchestrator_proc is not None and self.orchestrator_proc.poll() is None:
            return
        cmd = [
            sys.executable,
            "-m",
            "uvicorn",
            "app:app",
            "--app-dir",
            "services/orchestrator",
            "--host",
            self.config.orchestrator_host,
            "--port",
            str(self.config.orchestrator_port),
        ]
        self.orchestrator_proc = subprocess.Popen(
            cmd,
            stdout=sys.stderr,
            stderr=sys.stderr,
            text=True,
        )
        deadline = time.time() + self.config.startup_timeout_secs
        while time.time() < deadline:
            status, _ = _http_json(self.config, "GET", "/health")
            if status == 200:
                return
            if self.orchestrator_proc.poll() is not None:
                raise RuntimeError("orchestrator process exited during startup")
            time.sleep(0.25)
        raise RuntimeError("orchestrator did not become healthy before timeout")

    def tool_call(self, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        self.ensure_orchestrator()
        if name == "health":
            status, payload = _http_json(self.config, "GET", "/health")
            return _tool_result(status == 200, payload)
        if name == "memory.search":
            status, payload = _http_json(self.config, "POST", "/memory/search", arguments)
            return _tool_result(status == 200, payload)
        if name == "memory.write":
            status, payload = _http_json(self.config, "POST", "/memory/write", arguments)
            return _tool_result(status == 200, payload)
        return {
            "isError": True,
            "content": [{"type": "text", "text": f"Unknown tool: {name}"}],
        }


def _handle_signal(runtime: BridgeRuntime, signum: int, _frame: Any) -> None:
    _stderr(f"received signal {signum}; shutting down")
    runtime.stop()
    raise SystemExit(0)


def main() -> int:
    config = _load_config()
    runtime = BridgeRuntime(config)
    atexit.register(runtime.stop)
    signal.signal(signal.SIGTERM, lambda s, f: _handle_signal(runtime, s, f))
    signal.signal(signal.SIGINT, lambda s, f: _handle_signal(runtime, s, f))

    while True:
        try:
            message = _read_message()
        except Exception as exc:
            _stderr(f"failed to read MCP request: {exc}")
            break
        if message is None:
            break
        method = message.get("method")
        req_id = message.get("id")
        params = message.get("params") if isinstance(message.get("params"), dict) else {}

        if method == "notifications/initialized":
            continue
        if method == "initialize":
            protocol_version = params.get("protocolVersion") or DEFAULT_PROTOCOL_VERSION
            result = {
                "protocolVersion": protocol_version,
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": {"name": SERVER_NAME, "version": SERVER_VERSION},
            }
            _write_message(_jsonrpc_result(req_id, result))
            continue
        if method == "tools/list":
            _write_message(_jsonrpc_result(req_id, {"tools": TOOLS}))
            continue
        if method == "tools/call":
            name = str(params.get("name") or "").strip()
            arguments = params.get("arguments") if isinstance(params.get("arguments"), dict) else {}
            try:
                result = runtime.tool_call(name, arguments)
                _write_message(_jsonrpc_result(req_id, result))
            except Exception as exc:
                _write_message(_jsonrpc_error(req_id, -32000, f"tool call failed: {exc}"))
            continue

        _write_message(_jsonrpc_error(req_id, -32601, f"Method not found: {method}"))

    runtime.stop()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
