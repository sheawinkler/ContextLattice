#!/usr/bin/env python3
"""Shared agent-output contract registry loader and validator."""

from __future__ import annotations

import json
import os
from copy import deepcopy
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_REGISTRY_PATH = REPO_ROOT / "config" / "agent_contracts" / "agent_output_contracts.json"
REGISTRY_ENV = "CONTEXTLATTICE_AGENT_CONTRACTS_PATH"
PROVIDER_OVERFLOW_PATTERNS = (
    "array_above_max_length",
    "context length exceeded",
    "maximum context length",
    "max context length",
    "input array is too long",
    "oversized input",
)


def registry_path() -> Path:
    override = os.getenv(REGISTRY_ENV, "").strip()
    if override:
        return Path(override).expanduser()
    return DEFAULT_REGISTRY_PATH


def load_agent_contracts_registry(path: Path | None = None) -> dict[str, Any]:
    source = path or registry_path()
    payload = json.loads(source.read_text(encoding="utf-8"))
    if not isinstance(payload, dict):
        raise ValueError(f"{source} must contain a JSON object")
    contracts = payload.get("contracts")
    if not isinstance(contracts, dict) or not contracts:
        raise ValueError(f"{source} missing contracts object")
    return payload


def _contract(registry: dict[str, Any], contract_id: str) -> dict[str, Any]:
    contracts = registry.get("contracts") if isinstance(registry, dict) else None
    contract = contracts.get(contract_id) if isinstance(contracts, dict) else None
    if not isinstance(contract, dict):
        raise KeyError(f"missing contract {contract_id}")
    return contract


def anti_scheming_protocol(registry: dict[str, Any] | None = None) -> dict[str, Any]:
    registry = registry or load_agent_contracts_registry()
    fragments = registry.get("shared_fragments") if isinstance(registry, dict) else None
    protocol = fragments.get("anti_scheming_protocol") if isinstance(fragments, dict) else None
    if not isinstance(protocol, dict):
        raise KeyError("missing shared_fragments.anti_scheming_protocol")
    return deepcopy(protocol)


def _path_get(payload: Any, dotted_path: str) -> Any:
    current = payload
    for part in str(dotted_path or "").split("."):
        if not part:
            continue
        if not isinstance(current, dict) or part not in current:
            return None
        current = current[part]
    return current


def _matches_type(value: Any, expected: str) -> bool:
    expected = str(expected or "").strip().lower()
    if expected == "object":
        return isinstance(value, dict)
    if expected == "string":
        return isinstance(value, str)
    if expected == "bool":
        return isinstance(value, bool)
    if expected == "list":
        return isinstance(value, list)
    if expected == "list[string]":
        return isinstance(value, list) and all(isinstance(item, str) for item in value)
    if expected == "int":
        return isinstance(value, int) and not isinstance(value, bool)
    if expected == "number":
        return isinstance(value, (int, float)) and not isinstance(value, bool)
    return True


def _walk_forbidden_keys(value: Any, forbidden: set[str], path: str = "") -> list[dict[str, Any]]:
    findings: list[dict[str, Any]] = []
    if isinstance(value, dict):
        for key, item in value.items():
            key_text = str(key)
            current_path = f"{path}.{key_text}" if path else key_text
            if key_text in forbidden:
                findings.append({"reason": "forbidden_field_present", "path": current_path})
            findings.extend(_walk_forbidden_keys(item, forbidden, current_path))
    elif isinstance(value, list):
        for index, item in enumerate(value[:128]):
            current_path = f"{path}[{index}]" if path else f"[{index}]"
            findings.extend(_walk_forbidden_keys(item, forbidden, current_path))
    return findings


def _json_bytes(value: Any) -> int:
    return len(json.dumps(value, sort_keys=True, separators=(",", ":")).encode("utf-8"))


def _sanitize_provider_overflow_text(value: str) -> str:
    lowered = value.lower()
    if any(pattern in lowered for pattern in PROVIDER_OVERFLOW_PATTERNS):
        return "ContextLattice boundary reduced an oversized provider input before returning this payload."
    return value


def _clip_utf8(value: str, max_bytes: int) -> str:
    raw = value.encode("utf-8")
    if max_bytes <= 0 or len(raw) <= max_bytes:
        return value
    suffix = b"... [truncated]"
    if max_bytes <= len(suffix):
        return raw[:max_bytes].decode("utf-8", errors="ignore")
    prefix = raw[: max_bytes - len(suffix)].decode("utf-8", errors="ignore")
    return prefix + suffix.decode("ascii")


def _walk_string_bytes(value: Any, max_bytes: int, contract_id: str, path: str = "") -> list[dict[str, Any]]:
    findings: list[dict[str, Any]] = []
    if max_bytes <= 0:
        return findings
    if isinstance(value, dict):
        for key in sorted(value):
            next_path = f"{path}.{key}" if path else str(key)
            findings.extend(_walk_string_bytes(value[key], max_bytes, contract_id, next_path))
    elif isinstance(value, list):
        for item in value[:512]:
            findings.extend(_walk_string_bytes(item, max_bytes, contract_id, f"{path}[]"))
    elif isinstance(value, str):
        actual = len(value.encode("utf-8"))
        if actual > max_bytes:
            findings.append(
                {
                    "reason": "string_bytes_exceed_contract",
                    "path": path,
                    "bytes": actual,
                    "max_bytes": max_bytes,
                    "contract_id": contract_id,
                }
            )
    return findings


def _walk_list_items(value: Any, max_items: int, contract_id: str, path: str = "") -> list[dict[str, Any]]:
    findings: list[dict[str, Any]] = []
    if max_items <= 0:
        return findings
    if isinstance(value, dict):
        for key in sorted(value):
            next_path = f"{path}.{key}" if path else str(key)
            findings.extend(_walk_list_items(value[key], max_items, contract_id, next_path))
    elif isinstance(value, list):
        if len(value) > max_items:
            findings.append(
                {
                    "reason": "list_items_exceed_contract",
                    "path": path,
                    "items": len(value),
                    "max_items": max_items,
                    "contract_id": contract_id,
                }
            )
        for item in value[:512]:
            findings.extend(_walk_list_items(item, max_items, contract_id, f"{path}[]"))
    return findings


def _empty_boundary_counts() -> dict[str, int]:
    return {
        "strings_clipped": 0,
        "lists_clipped": 0,
        "optional_fields_compacted": 0,
        "boundary_passes": 0,
        "json_bytes_reduced": 0,
    }


def _merge_boundary_counts(*items: dict[str, Any] | None) -> dict[str, int]:
    merged = _empty_boundary_counts()
    for item in items:
        if not isinstance(item, dict):
            continue
        for key in merged:
            try:
                merged[key] += int(item.get(key) or 0)
            except Exception:
                continue
    return merged


def _boundary_omitted_counts(value: Any) -> dict[str, int]:
    counts = _empty_boundary_counts()

    def walk(item: Any) -> None:
        if isinstance(item, dict):
            for raw_key, nested in item.items():
                key = str(raw_key)
                if key == "omitted_by_boundary" and nested is True:
                    counts["optional_fields_compacted"] += 1
                elif key in {"clipped", "output_truncated", "_contextlattice_input_truncated"} and bool(nested):
                    counts["optional_fields_compacted"] += 1
                elif key in {"_truncated_items", "_truncated_keys"}:
                    try:
                        counts["lists_clipped"] += max(1, int(nested))
                    except Exception:
                        counts["lists_clipped"] += 1
                elif key == "_truncated":
                    counts["optional_fields_compacted"] += 1
                walk(nested)
        elif isinstance(item, list):
            for nested in item:
                walk(nested)
        elif isinstance(item, str) and item.endswith("... [truncated]"):
            counts["strings_clipped"] += 1

    walk(value)
    return counts


def _enforce_value_limits(value: Any, max_string_bytes: int, max_list_items: int, sanitize_overflow: bool) -> Any:
    if isinstance(value, dict):
        return {
            str(key): _enforce_value_limits(item, max_string_bytes, max_list_items, sanitize_overflow)
            for key, item in value.items()
        }
    if isinstance(value, list):
        items = value
        if max_list_items >= 0 and len(items) > max_list_items:
            items = items[:max_list_items]
        return [_enforce_value_limits(item, max_string_bytes, max_list_items, sanitize_overflow) for item in items]
    if isinstance(value, str):
        text = _sanitize_provider_overflow_text(value) if sanitize_overflow else value
        if max_string_bytes > 0:
            text = _clip_utf8(text, max_string_bytes)
        return text
    return value


def _compact_context_pack_payload(payload: dict[str, Any], keep: int) -> None:
    pack = payload.get("context_pack")
    if isinstance(pack, dict):
        for key in (
            "facts",
            "numericFacts",
            "numeric_facts",
            "citations",
            "results",
            "rankedEvidence",
            "ranked_evidence",
            "relevantDecisions",
            "relevant_decisions",
            "filesToRead",
            "files_to_read",
            "filesToAvoid",
            "files_to_avoid",
            "capabilitiesToUse",
            "capabilities_to_use",
            "runbooks",
            "knownFailureModes",
            "known_failure_modes",
            "commands",
            "acceptanceCriteria",
            "acceptance_criteria",
        ):
            if isinstance(pack.get(key), list):
                pack[key] = pack[key][:keep]
        sections = pack.get("prompt_sections")
        if isinstance(sections, dict):
            for key in ("evidence", "files_to_inspect", "commands", "checks", "risks", "capabilities", "constraints"):
                if isinstance(sections.get(key), list):
                    sections[key] = sections[key][:keep]
            for key in ("objective", "task", "mission", "goal", "next_action"):
                if isinstance(sections.get(key), str):
                    sections[key] = _clip_utf8(_sanitize_provider_overflow_text(sections[key]), 1200)
    if isinstance(payload.get("reference_prompt"), str):
        payload["reference_prompt"] = _clip_utf8(_sanitize_provider_overflow_text(payload["reference_prompt"]), 5000)
    if "retrieval" in payload:
        payload["retrieval"] = {"omitted_by_boundary": True}
    if isinstance(payload.get("warnings"), list):
        payload["warnings"] = payload["warnings"][: min(keep, 8)]


def _as_text_list(value: Any, limit: int = 16) -> list[str]:
    if not isinstance(value, list):
        return []
    out: list[str] = []
    for item in value[:limit]:
        text = str(item or "").strip()
        if text:
            out.append(text)
    return out


def _minimal_run_advisor(payload: dict[str, Any], registry: dict[str, Any]) -> dict[str, Any]:
    pack = payload.get("context_pack") if isinstance(payload.get("context_pack"), dict) else {}
    coverage = payload.get("source_coverage") if isinstance(payload.get("source_coverage"), dict) else None
    if coverage is None:
        coverage = pack.get("source_coverage") if isinstance(pack.get("source_coverage"), dict) else {}
    ranked = pack.get("ranked_evidence") if isinstance(pack.get("ranked_evidence"), list) else pack.get("rankedEvidence")
    ranked_count = len(ranked) if isinstance(ranked, list) else 0
    returned = _as_text_list(coverage.get("returned"), 16)
    pending = _as_text_list(coverage.get("pending"), 16)
    warming = _as_text_list(coverage.get("warming"), 16)
    failed = _as_text_list(coverage.get("failed"), 8)
    timed_out = _as_text_list(coverage.get("timed_out"), 8)
    complete = bool(coverage.get("complete"))
    reference_prompt = str(payload.get("reference_prompt") or "")
    score = 30 + min(30, ranked_count * 5) + min(20, len(returned) * 5)
    if len(reference_prompt) >= 300:
        score += 10
    if complete:
        score += 10
    else:
        score -= min(20, (len(pending) + len(warming) + len(failed) + len(timed_out)) * 4)
    score = max(0, min(100, score))
    state = "ready"
    posture = "ready"
    if not returned and ranked_count == 0 and (failed or timed_out) and not (pending or warming):
        state = "blocked"
        posture = "blocked"
    elif not returned and ranked_count == 0:
        state = "needs_context"
        posture = "needs_retrieval"
    elif not complete or score < 70:
        state = "usable_partial"
        posture = "partial_context"
    missing: list[str] = []
    if ranked_count == 0:
        missing.append("ranked_evidence")
    if not returned:
        missing.append("returned_sources")
    if not complete:
        missing.append("complete_source_coverage")
    repair = "Continue with the compiled prompt packet; rerun context retrieval only if the task needs fresher evidence."
    if pending or warming:
        repair = "Watch continuation events or rerun with --blocking when complete slow-source evidence is required."
    elif failed or timed_out:
        repair = "Retry with a narrower query, longer timeout, or smaller source set before making evidence-backed claims."
    advisor = {
        "ok": True,
        "schema_id": "run_advisor.v1",
        "posture": posture,
        "prompt_quality": {
            "score": score,
            "state": state,
            "ranked_evidence_count": ranked_count,
            "reference_prompt_chars": len(reference_prompt),
            "returned_source_count": len(returned),
            "complete": complete,
            "missing": missing,
        },
        "retrieval_advice": {
            "recommended_mode": "deep" if posture == "needs_retrieval" else str(payload.get("retrieval_mode") or pack.get("retrieval_mode") or "balanced"),
            "recommended_surface": "cli_for_local_agents",
            "alternate_surfaces": ["http_for_app_integrations", "mcp_for_tool_calling_hosts"],
            "rationale": ["bounded_cli_context_pack"],
            "blocking_recommended": bool(pending or warming),
        },
        "continuation": {
            "status": "partial" if pending or warming else ("failed" if failed or timed_out else "succeeded"),
            "token": "",
            "poll_url": "",
            "events_url": "",
            "pending_sources": pending,
            "warming_sources": warming,
            "failed_sources": failed,
            "timed_out_sources": timed_out,
            "budget_exceeded_sources": _as_text_list(coverage.get("budget_exceeded"), 8),
            "continuation_available": False,
            "modeled_progress": {},
            "repair_instruction": repair,
            "agent_followup_command": "",
            "agent_followup_endpoint": "",
            "agent_followup_transport": "none",
        },
        "objective_coherence": {
            "score": 0,
            "status": "missing",
            "signals": {
                "mission_present": False,
                "objective_present": False,
                "goal_present": False,
                "shared_terms": [],
                "query_token_count": 0,
                "context_token_count": 0,
            },
            "repair_instruction": "Carry the user objective, goal, and mission into the next prompt packet.",
        },
        "graph_quality": {
            "status": "not_sampled",
            "score": 0,
            "signals": {"edge_samples": 0},
            "recommendation": "Run contextlattice_memory_topology when graph evidence matters.",
        },
        "next_actions": [
            {
                "label": "send_reference_prompt" if state != "needs_context" else "rebuild_context_pack",
                "command": "use response.reference_prompt for the next model call" if state != "needs_context" else "contextlattice_pack '<query>' --mode deep --pretty",
                "reason": "The packet is bounded and shaped for agent prompt repackaging.",
            }
        ],
    }
    return attach_format_contract("run_advisor.v1", advisor, registry)


def _ensure_context_pack_run_advisor(payload: dict[str, Any], registry: dict[str, Any]) -> None:
    if not isinstance(payload, dict):
        return
    existing = payload.get("run_advisor")
    if isinstance(existing, dict) and existing:
        return
    advisor = _minimal_run_advisor(payload, registry)
    payload["run_advisor"] = advisor
    pack = payload.get("context_pack")
    if isinstance(pack, dict):
        pack.setdefault("run_advisor", advisor)
        pack.setdefault("runAdvisor", advisor)


def _compact_policy_payload(payload: dict[str, Any], keep: int) -> None:
    for key in ("mission", "objective", "goal", "query"):
        if isinstance(payload.get(key), str):
            payload[key] = _clip_utf8(_sanitize_provider_overflow_text(payload[key]), 2000)
    evidence = payload.get("evidence")
    if isinstance(evidence, dict):
        for key in ("primary_facts", "mission_facts"):
            if isinstance(evidence.get(key), list):
                evidence[key] = evidence[key][: max(5, keep)]
        if isinstance(evidence.get("mission_pack_error"), str):
            evidence["mission_pack_error"] = _clip_utf8(_sanitize_provider_overflow_text(evidence["mission_pack_error"]), 1000)
    handoff = payload.get("handoff")
    if isinstance(handoff, dict) and isinstance(handoff.get("handoff_prompt"), str):
        handoff["handoff_prompt"] = _clip_utf8(_sanitize_provider_overflow_text(handoff["handoff_prompt"]), 4000)
    objective_runtime = payload.get("objective_runtime")
    if isinstance(objective_runtime, dict):
        _compact_objective_runtime_payload(objective_runtime, keep)


def _compact_objective_runtime_payload(payload: dict[str, Any], keep: int) -> None:
    for key in ("mission", "objective", "goal"):
        if isinstance(payload.get(key), str):
            payload[key] = _clip_utf8(_sanitize_provider_overflow_text(payload[key]), 1600)
    if isinstance(payload.get("next_action"), str):
        payload["next_action"] = _clip_utf8(_sanitize_provider_overflow_text(payload["next_action"]), 1200)
    risk = payload.get("risk_or_blocker")
    if isinstance(risk, dict) and isinstance(risk.get("fastest_recovery_path"), str):
        risk["fastest_recovery_path"] = _clip_utf8(_sanitize_provider_overflow_text(risk["fastest_recovery_path"]), 1200)
    evidence = payload.get("evidence")
    if isinstance(evidence, dict):
        for key in ("required", "current"):
            if isinstance(evidence.get(key), list):
                evidence[key] = evidence[key][: max(3, keep)]


def _compact_preflight_payload(payload: dict[str, Any], keep: int) -> None:
    for key in ("context_pack", "mission_context_pack", "mission_pack"):
        value = payload.get(key)
        if isinstance(value, dict):
            _compact_context_pack_payload(value, keep)
            nested = value.get("payload")
            if isinstance(nested, dict):
                _compact_context_pack_payload(nested, keep)
    for key in ("scoped_search", "broadened_search", "status", "health"):
        if isinstance(payload.get(key), dict):
            payload[key] = {"omitted_by_boundary": True}
    policy = payload.get("policy_context_package")
    if isinstance(policy, dict):
        _compact_policy_payload(policy, keep)
    objective_runtime = payload.get("objective_runtime")
    if isinstance(objective_runtime, dict):
        _compact_objective_runtime_payload(objective_runtime, keep)


def enforce_contract_limits(
    contract_id: str,
    payload: dict[str, Any],
    registry: dict[str, Any] | None = None,
) -> dict[str, Any]:
    registry = registry or load_agent_contracts_registry()
    contract = _contract(registry, contract_id)
    if contract_id == "context_pack_response.v1":
        _ensure_context_pack_run_advisor(payload, registry)
    max_total = int(contract.get("max_total_json_bytes") or 0)
    max_string = int(contract.get("max_string_bytes") or 0)
    max_list = int(contract.get("max_list_items") or 0)
    sanitize_overflow = contract_id != "context_overflow_recovery.v1"
    out = _enforce_value_limits(deepcopy(payload), max_string, max_list if max_list > 0 else -1, sanitize_overflow)
    if not isinstance(out, dict) or max_total <= 0 or _json_bytes(out) <= max_total:
        return out if isinstance(out, dict) else dict(payload)

    for keep, string_cap, list_cap in ((16, 2048, 16), (8, 1024, 8), (5, 512, 8)):
        if contract_id == "context_pack_response.v1":
            _compact_context_pack_payload(out, keep)
        elif contract_id == "agent_preflight_response.v1":
            _compact_preflight_payload(out, keep)
        elif contract_id == "policy_context_package.v1":
            _compact_policy_payload(out, keep)
        elif contract_id == "objective_runtime_state.v1":
            _compact_objective_runtime_payload(out, keep)
        out = _enforce_value_limits(
            out,
            min(max_string, string_cap) if max_string else string_cap,
            min(max_list, list_cap) if max_list else list_cap,
            sanitize_overflow,
        )
        if _json_bytes(out) <= max_total:
            return out
    return out


def validate_agent_contract_payload(
    contract_id: str,
    payload: Any,
    registry: dict[str, Any] | None = None,
) -> list[dict[str, Any]]:
    registry = registry or load_agent_contracts_registry()
    contract = _contract(registry, contract_id)
    findings: list[dict[str, Any]] = []
    if not isinstance(payload, dict):
        return [{"reason": "payload_not_object", "contract_id": contract_id}]

    allowed = contract.get("allowed_fields")
    if isinstance(allowed, list):
        allowed_set = {str(item) for item in allowed}
        extra = sorted(set(payload) - allowed_set)
        for key in extra:
            findings.append({"reason": "unexpected_field", "field": key, "contract_id": contract_id})

    for field in contract.get("required_fields") or []:
        field_text = str(field)
        if field_text not in payload:
            findings.append({"reason": "missing_required_field", "field": field_text, "contract_id": contract_id})

    field_types = contract.get("field_types") if isinstance(contract.get("field_types"), dict) else {}
    for dotted_path, expected_type in field_types.items():
        value = _path_get(payload, str(dotted_path))
        if value is None:
            continue
        if not _matches_type(value, str(expected_type)):
            findings.append(
                {
                    "reason": "field_type_mismatch",
                    "path": str(dotted_path),
                    "expected": str(expected_type),
                    "actual": type(value).__name__,
                    "contract_id": contract_id,
                }
            )

    nested_required = contract.get("required_fields_by_path")
    if isinstance(nested_required, dict):
        for dotted_path, fields in nested_required.items():
            target = _path_get(payload, str(dotted_path))
            if not isinstance(target, dict):
                findings.append({"reason": "missing_required_object", "path": str(dotted_path), "contract_id": contract_id})
                continue
            for field in fields or []:
                field_text = str(field)
                if field_text not in target:
                    findings.append(
                        {
                            "reason": "missing_required_nested_field",
                            "path": f"{dotted_path}.{field_text}",
                            "contract_id": contract_id,
                        }
                    )

    for dotted_path in contract.get("required_true_paths") or []:
        value = _path_get(payload, str(dotted_path))
        if value is not True:
            findings.append({"reason": "required_true_path_not_true", "path": str(dotted_path), "contract_id": contract_id})

    for dotted_path in contract.get("required_false_paths") or []:
        value = _path_get(payload, str(dotted_path))
        if value is not False:
            findings.append({"reason": "required_false_path_not_false", "path": str(dotted_path), "contract_id": contract_id})

    contains = contract.get("required_string_contains")
    if isinstance(contains, dict):
        for dotted_path, needle in contains.items():
            value = _path_get(payload, str(dotted_path))
            if str(needle) not in str(value or ""):
                findings.append(
                    {
                        "reason": "required_string_missing",
                        "path": str(dotted_path),
                        "needle": str(needle),
                        "contract_id": contract_id,
                    }
                )

    min_items = contract.get("min_items")
    if isinstance(min_items, dict):
        for dotted_path, raw_min in min_items.items():
            value = _path_get(payload, str(dotted_path))
            try:
                min_count = int(raw_min)
            except Exception:
                min_count = 1
            if not isinstance(value, list) or len(value) < min_count:
                findings.append(
                    {
                        "reason": "list_min_items_not_met",
                        "path": str(dotted_path),
                        "min_items": min_count,
                        "actual": len(value) if isinstance(value, list) else 0,
                        "contract_id": contract_id,
                    }
                )

    max_total = int(contract.get("max_total_json_bytes") or 0)
    if max_total > 0:
        actual = _json_bytes(payload)
        if actual > max_total:
            findings.append(
                {
                    "reason": "json_bytes_exceed_contract",
                    "bytes": actual,
                    "max_bytes": max_total,
                    "contract_id": contract_id,
                    "payload_kind": contract.get("payload_kind"),
                }
            )

    max_string = int(contract.get("max_string_bytes") or 0)
    findings.extend(_walk_string_bytes(payload, max_string, contract_id))

    max_list = int(contract.get("max_list_items") or 0)
    findings.extend(_walk_list_items(payload, max_list, contract_id))

    max_bytes = contract.get("max_bytes_by_path")
    if isinstance(max_bytes, dict):
        for dotted_path, raw_max in max_bytes.items():
            value = _path_get(payload, str(dotted_path))
            if not isinstance(value, str):
                continue
            try:
                max_count = int(raw_max)
            except Exception:
                continue
            actual = len(value.encode("utf-8"))
            if actual > max_count:
                findings.append(
                    {
                        "reason": "string_bytes_exceed_contract",
                        "path": str(dotted_path),
                        "bytes": actual,
                        "max_bytes": max_count,
                        "contract_id": contract_id,
                    }
                )

    forbidden = {str(item) for item in contract.get("forbidden_fields") or [] if str(item)}
    if forbidden:
        if str(contract.get("forbidden_scope") or "recursive") == "root":
            for key in sorted(set(payload) & forbidden):
                findings.append({"reason": "forbidden_field_present", "path": key})
        else:
            findings.extend(_walk_forbidden_keys(payload, forbidden))
    if contract_id == "task_identity_reconciliation.v1":
        match_mode = str(payload.get("match_mode") or "").strip()
        abstention_modes = {"semantic_candidate", "ambiguous_semantic", "ambiguous_exact", "none"}
        confirmation_modes = {"semantic_candidate", "ambiguous_semantic", "ambiguous_exact"}
        if match_mode in abstention_modes:
            if payload.get("abstained") is not True:
                findings.append(
                    {"reason": "identity_abstention_required", "path": "abstained", "match_mode": match_mode, "contract_id": contract_id}
                )
            if str(payload.get("task_identity_id") or "").strip():
                findings.append(
                    {
                        "reason": "abstention_cannot_bind_identity",
                        "path": "task_identity_id",
                        "match_mode": match_mode,
                        "contract_id": contract_id,
                    }
                )
        if match_mode in confirmation_modes and payload.get("requires_confirmation") is not True:
            findings.append(
                {
                    "reason": "identity_confirmation_required",
                    "path": "requires_confirmation",
                    "match_mode": match_mode,
                    "contract_id": contract_id,
                }
            )
    return findings


def contract_metadata(contract_id: str, registry: dict[str, Any] | None = None) -> dict[str, Any]:
    registry = registry or load_agent_contracts_registry()
    contract = _contract(registry, contract_id)
    metadata = {
        "registry_id": str(registry.get("registry_id") or "contextlattice_agent_output_contracts"),
        "registry_version": int(registry.get("registry_version") or 0),
        "schema_id": contract_id,
        "contract_version": int(contract.get("contract_version") or 0),
        "required_output_mode": str(contract.get("required_output_mode") or "json_object"),
        "validator": str(registry.get("default_validator") or "contextlattice.boundary.v1"),
        "forbidden_fields": [str(item) for item in contract.get("forbidden_fields") or []],
        "contract_valid": False,
        "truncated": False,
        "omitted_counts": _empty_boundary_counts(),
        "validation": {"status": "pending", "errors": []},
    }
    for key in ("max_total_json_bytes", "max_string_bytes", "max_list_items"):
        value = int(contract.get(key) or 0)
        if value > 0:
            metadata[key] = value
    if isinstance(contract.get("max_bytes_by_path"), dict):
        metadata["max_bytes_by_path"] = deepcopy(contract["max_bytes_by_path"])
    return metadata


def stamp_validation(
    metadata: dict[str, Any],
    findings: list[dict[str, Any]],
    payload: dict[str, Any] | None = None,
    original_json_bytes: int = 0,
    bounded_json_bytes: int = 0,
    previous_counts: dict[str, Any] | None = None,
) -> dict[str, Any]:
    stamped = deepcopy(metadata)
    observed_counts = _boundary_omitted_counts(payload) if isinstance(payload, dict) else _empty_boundary_counts()
    if original_json_bytes > 0 and bounded_json_bytes > 0 and original_json_bytes > bounded_json_bytes:
        observed_counts["json_bytes_reduced"] += original_json_bytes - bounded_json_bytes
        observed_counts["boundary_passes"] += 1
    counts = _merge_boundary_counts(previous_counts, observed_counts)
    truncated = any(value > 0 for value in counts.values())
    stamped["validation"] = {
        "status": "failed" if findings else "passed",
        "errors": findings[:12],
    }
    stamped["contract_valid"] = not bool(findings)
    stamped["truncated"] = truncated
    stamped["omitted_counts"] = counts
    if original_json_bytes > 0:
        stamped["json_bytes_before_boundary"] = original_json_bytes
    if bounded_json_bytes > 0:
        stamped["json_bytes_after_boundary"] = bounded_json_bytes
        stamped["actual_json_bytes"] = bounded_json_bytes
    return stamped


def agent_contract_ids(registry: dict[str, Any] | None = None) -> list[str]:
    registry = registry or load_agent_contracts_registry()
    contracts = registry.get("contracts") if isinstance(registry, dict) else {}
    return sorted(str(contract_id) for contract_id in contracts.keys())


def attach_format_contract(
    contract_id: str,
    payload: dict[str, Any],
    registry: dict[str, Any] | None = None,
) -> dict[str, Any]:
    registry = registry or load_agent_contracts_registry()
    stamped = dict(payload)
    if contract_id == "context_pack_response.v1":
        _ensure_context_pack_run_advisor(stamped, registry)
    metadata = contract_metadata(contract_id, registry)
    previous_metadata = stamped.get("format_contract") if isinstance(stamped.get("format_contract"), dict) else {}
    stamped["format_contract"] = metadata
    previous_counts: dict[str, Any] | None = previous_metadata.get("omitted_counts") if isinstance(previous_metadata, dict) else None
    findings: list[dict[str, Any]] = []
    before = _json_bytes(stamped)
    after = before
    for _ in range(5):
        before = _json_bytes(stamped)
        stamped = enforce_contract_limits(contract_id, stamped, registry)
        after = _json_bytes(stamped)
        findings = validate_agent_contract_payload(contract_id, stamped, registry)
        stamped["format_contract"] = stamp_validation(metadata, findings, stamped, before, after, previous_counts)
        previous_counts = stamped["format_contract"].get("omitted_counts") if isinstance(stamped.get("format_contract"), dict) else previous_counts
        post_stamp_findings = validate_agent_contract_payload(contract_id, stamped, registry)
        if not post_stamp_findings:
            stamped["format_contract"] = stamp_validation(metadata, post_stamp_findings, stamped, before, after, previous_counts)
            previous_counts = stamped["format_contract"].get("omitted_counts") if isinstance(stamped.get("format_contract"), dict) else previous_counts
            findings = validate_agent_contract_payload(contract_id, stamped, registry)
            if findings:
                continue
            break
        findings = post_stamp_findings
    if findings:
        stamped["format_contract"] = stamp_validation(metadata, findings, stamped, before, after, previous_counts)
    return stamped


def preflight_contracts_summary(
    findings: list[dict[str, Any]] | None = None,
    payload: dict[str, Any] | None = None,
    original_json_bytes: int = 0,
    bounded_json_bytes: int = 0,
    previous_counts: dict[str, Any] | None = None,
) -> dict[str, Any]:
    registry = load_agent_contracts_registry()
    errors = findings or []
    preflight_contract = _contract(registry, "agent_preflight_response.v1")
    observed_counts = _boundary_omitted_counts(payload) if isinstance(payload, dict) else _empty_boundary_counts()
    if original_json_bytes > 0 and bounded_json_bytes > 0 and original_json_bytes > bounded_json_bytes:
        observed_counts["json_bytes_reduced"] += original_json_bytes - bounded_json_bytes
        observed_counts["boundary_passes"] += 1
    counts = _merge_boundary_counts(previous_counts, observed_counts)
    truncated = any(value > 0 for value in counts.values())
    summary = {
        "registry_id": str(registry.get("registry_id") or "contextlattice_agent_output_contracts"),
        "registry_version": int(registry.get("registry_version") or 0),
        "contracts": agent_contract_ids(registry),
        "max_total_json_bytes": int(preflight_contract.get("max_total_json_bytes") or 0),
        "max_string_bytes": int(preflight_contract.get("max_string_bytes") or 0),
        "max_list_items": int(preflight_contract.get("max_list_items") or 0),
        "contract_valid": not bool(errors),
        "truncated": truncated,
        "omitted_counts": counts,
        "validation": {
            "status": "failed" if errors else "passed",
            "errors": errors[:12],
        },
    }
    if original_json_bytes > 0:
        summary["json_bytes_before_boundary"] = original_json_bytes
    if bounded_json_bytes > 0:
        summary["json_bytes_after_boundary"] = bounded_json_bytes
        summary["actual_json_bytes"] = bounded_json_bytes
    return {
        **summary,
    }


def attach_preflight_contracts(
    payload: dict[str, Any],
    registry: dict[str, Any] | None = None,
) -> dict[str, Any]:
    registry = registry or load_agent_contracts_registry()
    response = dict(payload)
    previous_metadata = response.get("format_contracts") if isinstance(response.get("format_contracts"), dict) else {}
    response["format_contracts"] = preflight_contracts_summary()
    previous_counts: dict[str, Any] | None = previous_metadata.get("omitted_counts") if isinstance(previous_metadata, dict) else None
    findings: list[dict[str, Any]] = []
    before = _json_bytes(response)
    after = before
    for _ in range(5):
        before = _json_bytes(response)
        response = enforce_contract_limits("agent_preflight_response.v1", response, registry)
        after = _json_bytes(response)
        findings = validate_agent_contract_payload("agent_preflight_response.v1", response, registry)
        response["format_contracts"] = preflight_contracts_summary(findings, response, before, after, previous_counts)
        previous_counts = response["format_contracts"].get("omitted_counts") if isinstance(response.get("format_contracts"), dict) else previous_counts
        post_stamp_findings = validate_agent_contract_payload("agent_preflight_response.v1", response, registry)
        if not post_stamp_findings:
            response["format_contracts"] = preflight_contracts_summary(post_stamp_findings, response, before, after, previous_counts)
            previous_counts = response["format_contracts"].get("omitted_counts") if isinstance(response.get("format_contracts"), dict) else previous_counts
            findings = validate_agent_contract_payload("agent_preflight_response.v1", response, registry)
            if findings:
                continue
            break
        findings = post_stamp_findings
    if findings:
        response["format_contracts"] = preflight_contracts_summary(findings, response, before, after, previous_counts)
    return response
