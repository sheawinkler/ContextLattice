#!/usr/bin/env python3
"""Bounded runner-quality ledger helpers for ContextLattice task adapters."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
from collections import Counter, defaultdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[2]
SCRIPTS_DIR = REPO_ROOT / "scripts"

if str(SCRIPTS_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_DIR))

from agent_contracts import attach_format_contract  # noqa: E402

SCHEMA_ID = "runner_quality_sample.v1"
DEFAULT_MAX_BYTES = 2 * 1024 * 1024
DEFAULT_MAX_SAMPLES = 1000

SECRET_PATTERNS = (
    re.compile(r"Bearer\s+[A-Za-z0-9._~+/=-]{16,}", re.IGNORECASE),
    re.compile(r"sk-[A-Za-z0-9_-]{12,}"),
    re.compile(r"(?<![A-Za-z0-9])[A-Za-z0-9][A-Za-z0-9_-]{47,}(?![A-Za-z0-9])"),
)


def now_iso() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _env_int(name: str, default: int, minimum: int, maximum: int) -> int:
    raw = str(os.getenv(name, "")).strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError:
        return default
    return min(max(value, minimum), maximum)


def ledger_path() -> Path:
    explicit = (
        os.getenv("CONTEXTLATTICE_RUNNER_QUALITY_LEDGER_PATH", "").strip()
        or os.getenv("CONTEXTLATTICE_RUNNER_QUALITY_LEDGER", "").strip()
    )
    if explicit:
        return Path(explicit).expanduser()
    root = os.getenv("GO_MEMORY_STORE_ROOT", "").strip() or os.getenv("MEMORY_BANK_ROOT", "").strip()
    if root:
        return Path(root).expanduser() / "_contextlattice" / "runner_quality_ledger.ndjson"
    return Path(".data") / "orchestrator" / "runner_quality_ledger.ndjson"


def ledger_limits() -> tuple[int, int]:
    return (
        _env_int("CONTEXTLATTICE_RUNNER_QUALITY_LEDGER_MAX_BYTES", DEFAULT_MAX_BYTES, 64 * 1024, 64 * 1024 * 1024),
        _env_int("CONTEXTLATTICE_RUNNER_QUALITY_LEDGER_MAX_SAMPLES", DEFAULT_MAX_SAMPLES, 20, 20000),
    )


def redact_text(value: str) -> str:
    text = str(value or "")
    for pattern in SECRET_PATTERNS:
        text = pattern.sub("[REDACTED]", text)
    return text


def stable_hash(value: Any) -> str:
    text = redact_text(json.dumps(value, sort_keys=True, separators=(",", ":"), default=str))
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:24]


def _safe_int(value: Any, default: int = 0) -> int:
    try:
        if value is None or value == "":
            return default
        return int(value)
    except Exception:
        return default


def _safe_float(value: Any, default: float = 0.0) -> float:
    try:
        if value is None or value == "":
            return default
        return float(value)
    except Exception:
        return default


def _safe_bool(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    return str(value or "").strip().lower() in {"1", "true", "yes", "on"}


def _string_list(value: Any, limit: int = 16) -> list[str]:
    if not isinstance(value, list):
        return []
    output: list[str] = []
    for item in value[:limit]:
        token = str(item or "").strip()
        if token:
            output.append(token[:200])
    return output


def compact_context_quality(bundle: dict[str, Any]) -> dict[str, Any]:
    quality = bundle.get("context_pack_quality") if isinstance(bundle.get("context_pack_quality"), dict) else {}
    if not quality:
        pack = bundle.get("context_pack") if isinstance(bundle.get("context_pack"), dict) else {}
        quality = pack.get("context_pack_quality") if isinstance(pack.get("context_pack_quality"), dict) else {}
    return {
        "sample_id": str(quality.get("sample_id") or ""),
        "quality_score": _safe_int(quality.get("quality_score"), 0),
        "confidence": str(quality.get("confidence") or ""),
        "calibration_grade": str(quality.get("calibration_grade") or ""),
        "exact_prompt_tokens_saved": _safe_int(quality.get("exact_prompt_tokens_saved"), 0),
        "modeled_inference_tokens_avoided": _safe_int(quality.get("modeled_inference_tokens_avoided"), 0),
        "modeled_extra_calls_avoided": _safe_float(quality.get("modeled_extra_calls_avoided"), 0.0),
        "tokenizer_exact": _safe_bool(quality.get("tokenizer_exact")),
    }


def compact_token_impact(bundle: dict[str, Any], result: dict[str, Any]) -> dict[str, Any]:
    token_impact = bundle.get("token_impact") if isinstance(bundle.get("token_impact"), dict) else {}
    metadata = result.get("metadata") if isinstance(result.get("metadata"), dict) else {}
    provider_usage = metadata.get("provider_usage") if isinstance(metadata.get("provider_usage"), dict) else {}
    return {
        "saved_tokens_estimate": _safe_int(token_impact.get("saved_tokens_estimate"), 0),
        "packed_tokens_estimate": _safe_int(token_impact.get("packed_tokens_estimate"), 0),
        "tokenizer_exact": _safe_bool(token_impact.get("tokenizer_exact")),
        "provider_prompt_tokens": _safe_int(provider_usage.get("prompt_tokens"), 0),
        "provider_completion_tokens": _safe_int(provider_usage.get("completion_tokens"), 0),
        "provider_total_tokens": _safe_int(provider_usage.get("total_tokens"), 0),
    }


def feedback_from_task(task: dict[str, Any]) -> dict[str, Any]:
    payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
    rating = payload.get("runner_quality_rating", payload.get("quality_rating"))
    label = payload.get("runner_feedback", payload.get("feedback"))
    return {
        "present": rating is not None or label is not None,
        "rating": _safe_int(rating, 0),
        "label": redact_text(str(label or ""))[:500],
        "source": "task_payload" if rating is not None or label is not None else "",
    }


def normalize_task_class(value: Any) -> str:
    text = str(value or "").strip().lower()
    if not text:
        return "general"
    text = re.sub(r"[^a-z0-9_.:/-]+", "-", text)
    text = re.sub(r"-{2,}", "-", text).strip("-_.:/")
    return text[:80] or "general"


def task_class_from_task(task: dict[str, Any]) -> str:
    payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
    for key in ("task_class", "taskClass", "runner_task_class", "runnerTaskClass", "role", "intent", "operation"):
        value = payload.get(key)
        if value is not None:
            return normalize_task_class(value)
    for key in ("task_class", "taskClass", "role", "intent", "operation"):
        value = task.get(key)
        if value is not None:
            return normalize_task_class(value)
    return "general"


def build_runner_quality_sample(
    *,
    task: dict[str, Any],
    agent: str,
    result: dict[str, Any],
    context_bundle: dict[str, Any],
    task_status: str,
    message: str,
    route_payload: dict[str, Any] | None = None,
) -> dict[str, Any]:
    metadata = result.get("metadata") if isinstance(result.get("metadata"), dict) else {}
    lifecycle = context_bundle.get("lifecycle") if isinstance(context_bundle.get("lifecycle"), dict) else {}
    warnings = result.get("warnings") if isinstance(result.get("warnings"), list) else []
    artifacts = result.get("artifacts") if isinstance(result.get("artifacts"), list) else []
    quality = compact_context_quality(context_bundle)
    token_impact = compact_token_impact(context_bundle, result)
    route = route_payload if isinstance(route_payload, dict) else {}
    runner_status = str(result.get("status") or "").strip().lower()
    ok = bool(result.get("ok"))
    duration = _safe_float(result.get("duration_secs"), 0.0)
    outcome = {
        "task_status": str(task_status or ""),
        "runner_status": runner_status,
        "first_pass_success": ok and runner_status == "succeeded",
        "blocked": runner_status in {"blocked", "missing_binary", "invalid_task", "timed_out", "skipped"},
        "failed": (not ok) and runner_status not in {"blocked", "missing_binary", "invalid_task", "timed_out", "skipped"},
        "retry_count": _safe_int(metadata.get("retry_count"), 0),
        "observed_followup_tokens": _safe_int(metadata.get("observed_followup_tokens"), 0),
    }
    sample = {
        "schema_id": SCHEMA_ID,
        "captured_at": now_iso(),
        "runner": str(result.get("runner") or agent),
        "agent": str(result.get("agent") or agent),
        "agent_id": str(result.get("agent_id") or metadata.get("agent_id") or ""),
        "task_id": str(result.get("task_id") or task.get("id") or ""),
        "project": str(result.get("project") or task.get("project") or "_global"),
        "task_class": task_class_from_task(task),
        "status": runner_status,
        "ok": ok,
        "exit_code": _safe_int(result.get("exit_code"), 0),
        "duration_secs": round(duration, 3),
        "context_pack_quality": quality,
        "token_impact": token_impact,
        "outcome": outcome,
        "feedback": feedback_from_task(task),
        "metadata": {
            "adapter": metadata.get("adapter"),
            "context_pack_quality_sample_id": quality.get("sample_id"),
            "summary_hash": stable_hash(result.get("summary") or message),
            "stdout_tail_hash": stable_hash(result.get("stdout_tail") or ""),
            "stderr_tail_hash": stable_hash(result.get("stderr_tail") or ""),
            "warning_count": len(warnings),
            "artifact_count": len(artifacts),
            "retrieval_status": str(lifecycle.get("status") or ""),
            "retrieval_result_state": str(lifecycle.get("result_state") or ""),
            "context_degraded": bool(lifecycle.get("degraded", False)),
            "pending_sources": _string_list(lifecycle.get("pending_sources"), 8),
            "route_provider": str(route.get("provider") or ""),
            "route_reason_hash": stable_hash(route.get("reason") or ""),
            "quality_basis": "context_pack_quality_sample" if quality.get("sample_id") else "absent",
        },
    }
    sample["sample_id"] = sample_id(sample)
    return attach_format_contract(SCHEMA_ID, sample)


def _read_rows(path: Path, limit: int | None = None) -> tuple[list[dict[str, Any]], int]:
    if not path.exists():
        return [], 0
    rows: list[dict[str, Any]] = []
    parse_errors = 0
    with path.open("r", encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line:
                continue
            try:
                parsed = json.loads(line)
            except json.JSONDecodeError:
                parse_errors += 1
                continue
            if isinstance(parsed, dict) and parsed.get("schema_id") == SCHEMA_ID:
                rows.append(parsed)
    if limit and limit > 0:
        rows = rows[-limit:]
    return rows, parse_errors


def _prune(path: Path, max_bytes: int, max_samples: int) -> None:
    if not path.exists():
        return
    if path.stat().st_size <= max_bytes:
        rows, _ = _read_rows(path, max_samples)
        if len(rows) < max_samples:
            return
    else:
        rows, _ = _read_rows(path, max_samples)
    encoded: list[bytes] = []
    total = 0
    for row in reversed(rows):
        raw = (json.dumps(row, sort_keys=True, separators=(",", ":")) + "\n").encode("utf-8")
        if encoded and total + len(raw) > max_bytes:
            break
        encoded.append(raw)
        total += len(raw)
    encoded.reverse()
    tmp = path.with_suffix(path.suffix + ".tmp")
    with tmp.open("wb") as handle:
        for raw in encoded:
            handle.write(raw)
    tmp.replace(path)


def record_sample(sample: dict[str, Any], path: Path | None = None) -> dict[str, Any]:
    target = path or ledger_path()
    max_bytes, max_samples = ledger_limits()
    target.parent.mkdir(parents=True, exist_ok=True)
    row = json.dumps(sample, sort_keys=True, separators=(",", ":"))
    with target.open("a", encoding="utf-8") as handle:
        handle.write(row + "\n")
    _prune(target, max_bytes, max_samples)
    return {
        "enabled": True,
        "path": str(target),
        "max_bytes": max_bytes,
        "max_samples": max_samples,
        "last_write_at": now_iso(),
        "sample_id": sample_id(sample),
    }


def sample_id(sample: dict[str, Any]) -> str:
    seed = "|".join(
        [
            str(sample.get("captured_at") or ""),
            str(sample.get("runner") or ""),
            str(sample.get("task_id") or ""),
            str(sample.get("status") or ""),
        ]
    )
    return "rqs_" + hashlib.sha256(seed.encode("utf-8")).hexdigest()[:24]


def record_runner_quality(
    *,
    task: dict[str, Any],
    agent: str,
    result: dict[str, Any],
    context_bundle: dict[str, Any],
    task_status: str,
    message: str,
    route_payload: dict[str, Any] | None = None,
) -> tuple[dict[str, Any], dict[str, Any]]:
    sample = build_runner_quality_sample(
        task=task,
        agent=agent,
        result=result,
        context_bundle=context_bundle,
        task_status=task_status,
        message=message,
        route_payload=route_payload,
    )
    storage = record_sample(sample)
    return sample, storage


def _row_task_class(row: dict[str, Any]) -> str:
    metadata = row.get("metadata") if isinstance(row.get("metadata"), dict) else {}
    return normalize_task_class(row.get("task_class") or metadata.get("task_class"))


def _recommendations(by_runner: dict[str, dict[str, Any]], sample_count: int, task_class: str) -> dict[str, Any]:
    candidates = []
    for runner, metrics in sorted(by_runner.items()):
        total = _safe_int(metrics.get("sample_count"), 0)
        if total <= 0:
            continue
        success_rate = _safe_float(metrics.get("success_rate"), 0.0)
        blocked_rate = _safe_float(metrics.get("blocked_rate"), 0.0)
        failure_rate = _safe_float(metrics.get("failure_rate"), 0.0)
        quality = _safe_float(metrics.get("average_context_quality_score"), 0.0)
        duration = _safe_float(metrics.get("average_duration_secs"), 0.0)
        score = (success_rate * 100.0) - (blocked_rate * 24.0) - (failure_rate * 38.0) + min(quality, 100.0) * 0.08
        if duration > 0:
            score -= min(duration, 300.0) * 0.015
        candidates.append(
            {
                "runner": runner,
                "score": round(score, 3),
                "sample_count": total,
                "success_rate": success_rate,
                "blocked_rate": blocked_rate,
                "failure_rate": failure_rate,
                "average_duration_secs": duration,
                "reason": (
                    f"{success_rate:.0%} success, {blocked_rate:.0%} blocked, "
                    f"{failure_rate:.0%} failed across {total} comparable sample(s)"
                ),
            }
        )
    candidates.sort(key=lambda item: (-_safe_float(item.get("score"), 0.0), -_safe_int(item.get("sample_count"), 0), str(item.get("runner") or "")))
    comparable_runners = sum(1 for item in candidates if _safe_int(item.get("sample_count"), 0) >= 2)
    if sample_count < 3:
        confidence = "insufficient_samples"
    elif sample_count < 10 or comparable_runners < 2:
        confidence = "low"
    elif sample_count < 30:
        confidence = "medium"
    else:
        confidence = "high"
    return {
        "mode": "advisor_only",
        "basis": "observed_bounded_runner_quality_samples",
        "task_class": task_class or "all",
        "minimum_samples_per_runner": 5,
        "confidence": confidence,
        "top_runner": str(candidates[0].get("runner") or "") if candidates else "",
        "candidates": candidates[:5],
        "guardrails": [
            "Never dispatch or mutate automatically from this summary.",
            "Compare only similar task_class samples before preferring a runner.",
            "missing_binary, auth, or blocked statuses are readiness signals, not proof that a runner is low quality.",
            "Use operator judgment and task constraints before selecting Pi, Droid, Codex, or another adapter.",
        ],
    }


def summarize(rows: list[dict[str, Any]], parse_errors: int = 0, task_class: str = "") -> dict[str, Any]:
    all_rows = rows
    requested_task_class = normalize_task_class(task_class) if task_class else ""
    if requested_task_class:
        rows = [row for row in rows if _row_task_class(row) == requested_task_class]
    by_runner: dict[str, dict[str, Any]] = {}
    status_counts = Counter(str(row.get("status") or "unknown") for row in rows)
    task_class_counts = Counter(_row_task_class(row) for row in all_rows)
    runner_rows: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for row in rows:
        runner_rows[str(row.get("runner") or "unknown")].append(row)
    for runner, items in sorted(runner_rows.items()):
        total = len(items)
        successes = sum(1 for row in items if bool(row.get("ok")) and row.get("status") == "succeeded")
        blocked = sum(1 for row in items if str(row.get("status") or "") in {"blocked", "missing_binary", "invalid_task", "timed_out", "skipped"})
        failed = total - successes - blocked
        duration_total = sum(_safe_float(row.get("duration_secs"), 0.0) for row in items)
        quality_scores = [
            _safe_int((row.get("context_pack_quality") or {}).get("quality_score"), 0)
            for row in items
            if isinstance(row.get("context_pack_quality"), dict)
            and _safe_int((row.get("context_pack_quality") or {}).get("quality_score"), 0) > 0
        ]
        exact_saved = sum(
            _safe_int((row.get("context_pack_quality") or {}).get("exact_prompt_tokens_saved"), 0)
            for row in items
            if isinstance(row.get("context_pack_quality"), dict)
        )
        modeled_avoided = sum(
            _safe_int((row.get("context_pack_quality") or {}).get("modeled_inference_tokens_avoided"), 0)
            for row in items
            if isinstance(row.get("context_pack_quality"), dict)
        )
        by_runner[runner] = {
            "sample_count": total,
            "success_count": successes,
            "blocked_count": blocked,
            "failed_count": failed,
            "success_rate": round(successes / total, 3) if total else 0,
            "blocked_rate": round(blocked / total, 3) if total else 0,
            "failure_rate": round(failed / total, 3) if total else 0,
            "average_duration_secs": round(duration_total / total, 3) if total else 0,
            "average_context_quality_score": round(sum(quality_scores) / len(quality_scores), 1) if quality_scores else 0,
            "exact_prompt_tokens_saved": exact_saved,
            "modeled_inference_tokens_avoided": modeled_avoided,
        }
    return {
        "schema_id": "contextlattice_runner_quality_summary.v1",
        "updated_at": now_iso(),
        "sample_count": len(rows),
        "total_sample_count": len(all_rows),
        "filtered": bool(requested_task_class),
        "task_class": requested_task_class or "all",
        "parse_errors": parse_errors,
        "by_status": dict(status_counts),
        "by_task_class": dict(task_class_counts),
        "by_runner": by_runner,
        "recommendations": _recommendations(by_runner, len(rows), requested_task_class or "all"),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Summarize the bounded ContextLattice runner-quality ledger.")
    parser.add_argument("--ledger", default="", help="Override runner-quality ledger path.")
    parser.add_argument("--limit", type=int, default=500)
    parser.add_argument("--task-class", default="", help="Filter/advice scope for comparable runner samples.")
    parser.add_argument("--pretty", action="store_true")
    args = parser.parse_args()
    path = Path(args.ledger).expanduser() if args.ledger else ledger_path()
    rows, parse_errors = _read_rows(path, args.limit)
    payload = summarize(rows, parse_errors=parse_errors, task_class=args.task_class)
    payload["storage"] = {
        "path": str(path),
        "exists": path.exists(),
        "bytes": path.stat().st_size if path.exists() else 0,
    }
    print(json.dumps(payload, indent=2 if args.pretty else None, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
