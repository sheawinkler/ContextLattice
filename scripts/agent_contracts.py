#!/usr/bin/env python3
"""Shared agent-output contract registry loader and validator."""

from __future__ import annotations

import hashlib
import json
import math
import os
import re
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
MIN_AGENT_CONTRACT_INT = -(1 << 63)
MAX_AGENT_CONTRACT_INT = (1 << 63) - 1
MAX_PUBLIC_TRACE_ID_BYTES = 47
MAX_RETRIEVAL_PROOF_JSON_DEPTH = 64


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


def _path_lookup(payload: Any, dotted_path: str) -> tuple[bool, Any]:
    current = payload
    for part in str(dotted_path or "").split("."):
        if not part:
            continue
        if not isinstance(current, dict) or part not in current:
            return False, None
        current = current[part]
    return True, current


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
        if isinstance(value, bool):
            return False
        if isinstance(value, int):
            return MIN_AGENT_CONTRACT_INT <= value <= MAX_AGENT_CONTRACT_INT
        if isinstance(value, float):
            return (
                math.isfinite(value)
                and value.is_integer()
                and MIN_AGENT_CONTRACT_INT <= value < float(1 << 63)
            )
        return False
    if expected == "number":
        if isinstance(value, bool):
            return False
        if isinstance(value, int):
            return MIN_AGENT_CONTRACT_INT <= value <= MAX_AGENT_CONTRACT_INT
        return isinstance(value, float) and math.isfinite(value)
    return True


def _canonical_agent_contract_field_key(value: Any) -> str:
    return "".join(character for character in str(value).strip().lower() if character.isalnum())


def _walk_forbidden_keys(value: Any, forbidden: set[str], path: str = "") -> list[dict[str, Any]]:
    findings: list[dict[str, Any]] = []
    if isinstance(value, dict):
        for key, item in value.items():
            key_text = str(key)
            current_path = f"{path}.{key_text}" if path else key_text
            if _canonical_agent_contract_field_key(key_text) in forbidden:
                findings.append({"reason": "forbidden_field_present", "path": current_path})
            findings.extend(_walk_forbidden_keys(item, forbidden, current_path))
    elif isinstance(value, list):
        for index, item in enumerate(value):
            current_path = f"{path}[{index}]" if path else f"[{index}]"
            findings.extend(_walk_forbidden_keys(item, forbidden, current_path))
    return findings


def _json_bytes(value: Any) -> int:
    try:
        return len(agent_contract_go_json(value))
    except (TypeError, ValueError, OverflowError, UnicodeEncodeError, RecursionError):
        return -1


def _go_json_float(value: float) -> str:
    """Render a finite float with encoding/json's float64 thresholds."""

    if not math.isfinite(value):
        raise ValueError("nonfinite JSON number")
    if value == 0:
        return "-0" if math.copysign(1.0, value) < 0 else "0"
    rendered = repr(value).lower()
    absolute = abs(value)
    if absolute < 1e-6 or absolute >= 1e21:
        mantissa, exponent = rendered.split("e", 1)
        if mantissa.endswith(".0"):
            mantissa = mantissa[:-2]
        return f"{mantissa}e{int(exponent):+d}"
    if "e" in rendered:
        mantissa, exponent_text = rendered.split("e", 1)
        exponent = int(exponent_text)
        sign = ""
        if mantissa.startswith("-"):
            sign = "-"
            mantissa = mantissa[1:]
        whole, _, fraction = mantissa.partition(".")
        digits = whole + fraction
        decimal_at = len(whole) + exponent
        if decimal_at <= 0:
            rendered = sign + "0." + ("0" * -decimal_at) + digits
        elif decimal_at >= len(digits):
            rendered = sign + digits + ("0" * (decimal_at - len(digits)))
        else:
            rendered = sign + digits[:decimal_at] + "." + digits[decimal_at:]
    if rendered.endswith(".0"):
        rendered = rendered[:-2]
    return rendered


def _go_json_string(value: str) -> str:
    value.encode("utf-8")
    return (
        json.dumps(value, ensure_ascii=False, allow_nan=False)
        .replace("<", "\\u003c")
        .replace(">", "\\u003e")
        .replace("&", "\\u0026")
        .replace("\u2028", "\\u2028")
        .replace("\u2029", "\\u2029")
    )


def _write_agent_contract_go_json(value: Any, depth: int = 0) -> str:
    if depth > MAX_RETRIEVAL_PROOF_JSON_DEPTH:
        raise ValueError("agent contract JSON exceeds maximum depth")
    if value is None:
        return "null"
    if value is True:
        return "true"
    if value is False:
        return "false"
    if isinstance(value, int):
        if value < MIN_AGENT_CONTRACT_INT or value > MAX_AGENT_CONTRACT_INT:
            raise ValueError("agent contract integer exceeds signed int64")
        return str(value)
    if isinstance(value, float):
        return _go_json_float(value)
    if isinstance(value, str):
        return _go_json_string(value)
    if isinstance(value, list):
        return "[" + ",".join(_write_agent_contract_go_json(item, depth + 1) for item in value) + "]"
    if isinstance(value, dict):
        if not all(isinstance(key, str) for key in value):
            raise TypeError("agent contract object keys must be strings")
        return "{" + ",".join(
            _go_json_string(key) + ":" + _write_agent_contract_go_json(value[key], depth + 1)
            for key in sorted(value)
        ) + "}"
    raise TypeError(f"unsupported agent contract JSON type {type(value).__name__}")


def agent_contract_go_json(value: Any) -> bytes:
    """Encode the shared contract domain exactly like Go encoding/json."""

    return _write_agent_contract_go_json(value).encode("utf-8")


def agent_contract_json_domain_valid(value: Any, depth: int = 0) -> bool:
    """Recognize the exact finite, signed-int64, UTF-8 JSON contract domain."""

    if depth > MAX_RETRIEVAL_PROOF_JSON_DEPTH:
        return False
    if value is None or isinstance(value, bool):
        return True
    if isinstance(value, int):
        return MIN_AGENT_CONTRACT_INT <= value <= MAX_AGENT_CONTRACT_INT
    if isinstance(value, float):
        return math.isfinite(value)
    if isinstance(value, str):
        try:
            value.encode("utf-8")
        except UnicodeEncodeError:
            return False
        return True
    if isinstance(value, list):
        return all(agent_contract_json_domain_valid(item, depth + 1) for item in value)
    if isinstance(value, dict):
        return all(
            isinstance(key, str)
            and agent_contract_json_domain_valid(key, depth + 1)
            and agent_contract_json_domain_valid(item, depth + 1)
            for key, item in value.items()
        )
    return False


def _canonical_retrieval_proof_json(value: Any) -> bytes:
    if not agent_contract_json_domain_valid(value):
        raise ValueError("retrieval proof is outside the canonical JSON domain")
    return json.dumps(
        value,
        allow_nan=False,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")


def _sanitize_provider_overflow_text(value: str) -> str:
    lowered = value.lower()
    if any(pattern in lowered for pattern in PROVIDER_OVERFLOW_PATTERNS):
        return "ContextLattice boundary reduced an oversized provider input before returning this payload."
    return value


def _clip_utf8(value: str, max_bytes: int) -> str:
    raw = value.encode("utf-8", errors="replace")
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
        actual = len(value.encode("utf-8", errors="replace"))
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


def _retrieval_proof_count(value: Any) -> int:
    if (
        isinstance(value, bool)
        or not isinstance(value, int)
        or value < 0
        or value > MAX_AGENT_CONTRACT_INT
    ):
        return 0
    return value


def _exact_contract_integer(value: Any, expected: Any) -> bool:
    return (
        isinstance(value, int)
        and not isinstance(value, bool)
        and -(1 << 63) <= value <= (1 << 63) - 1
        and isinstance(expected, int)
        and not isinstance(expected, bool)
        and value == expected
    )


def agent_contract_envelope_attestation_valid(
    contract_id: str,
    payload: Any,
    registry: dict[str, Any] | None = None,
) -> bool:
    if not isinstance(payload, dict) or not agent_contract_json_domain_valid(payload):
        return False
    try:
        registry = registry or load_agent_contracts_registry()
        contract = _contract(registry, contract_id)
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError):
        return False
    metadata = payload.get("format_contract")
    validation = metadata.get("validation") if isinstance(metadata, dict) else None
    actual = _json_bytes(payload)
    return bool(
        not validate_agent_contract_payload(contract_id, payload, registry)
        and isinstance(metadata, dict)
        and isinstance(validation, dict)
        and metadata.get("registry_id") == registry.get("registry_id")
        and _exact_contract_integer(metadata.get("registry_version"), registry.get("registry_version"))
        and metadata.get("schema_id") == contract_id
        and _exact_contract_integer(metadata.get("contract_version"), contract.get("contract_version"))
        and metadata.get("required_output_mode") == contract.get("required_output_mode")
        and metadata.get("validator") == str(registry.get("default_validator") or "contextlattice.boundary.v1")
        and metadata.get("contract_valid") is True
        and validation.get("status") == "passed"
        and validation.get("errors") == []
        and _exact_contract_integer(metadata.get("max_total_json_bytes"), contract.get("max_total_json_bytes"))
        and _exact_contract_integer(metadata.get("max_string_bytes"), contract.get("max_string_bytes"))
        and _exact_contract_integer(metadata.get("max_list_items"), contract.get("max_list_items"))
        and actual > 0
        and actual <= int(contract.get("max_total_json_bytes") or 0)
        and _exact_contract_integer(metadata.get("actual_json_bytes"), actual)
    )


OBJECTIVE_RUNTIME_ATTESTATION_KIND = "objective_runtime_attestation"


def _objective_runtime_authority_digest(
    *,
    session_id: str,
    project: str,
    agent_id: str,
    runtime_agent: str,
) -> str:
    authority = {
        "agent_id": agent_id,
        "project": project,
        "runtime_agent": runtime_agent,
        "session_id": session_id,
    }
    encoded = json.dumps(authority, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def build_objective_runtime_attestation(
    runtime: Any,
    *,
    session_id: str,
    project: str,
    agent_id: str,
    runtime_agent: str,
    registry: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Build a private, identity-bound receipt for a validated objective runtime."""

    if not all(isinstance(value, str) and value for value in (session_id, project, agent_id, runtime_agent)):
        return {}
    registry = registry or load_agent_contracts_registry()
    if (
        not isinstance(runtime, dict)
        or runtime.get("session_id") != session_id
        or runtime.get("project") != project
        or runtime.get("agent_id") != agent_id
        or runtime.get("agent") != runtime_agent
        or not agent_contract_envelope_attestation_valid("objective_runtime_state.v1", runtime, registry)
    ):
        return {}
    contract = _contract(registry, "objective_runtime_state.v1")
    metadata = runtime.get("format_contract") if isinstance(runtime.get("format_contract"), dict) else {}
    try:
        canonical = agent_contract_go_json(runtime)
    except (TypeError, ValueError, OverflowError, UnicodeEncodeError, RecursionError):
        return {}
    return {
        "kind": OBJECTIVE_RUNTIME_ATTESTATION_KIND,
        "contract_id": "objective_runtime_state.v1",
        "registry_id": registry.get("registry_id"),
        "registry_version": registry.get("registry_version"),
        "contract_version": contract.get("contract_version"),
        "actual_json_bytes": metadata.get("actual_json_bytes"),
        "runtime_agent": runtime_agent,
        "contract_valid": True,
        "authority_bound": True,
        "authority_digest": _objective_runtime_authority_digest(
            session_id=session_id,
            project=project,
            agent_id=agent_id,
            runtime_agent=runtime_agent,
        ),
        "canonical_digest": "sha256:" + hashlib.sha256(canonical).hexdigest(),
    }


def objective_runtime_attestation_valid(
    receipt: Any,
    *,
    session_id: str,
    project: str,
    agent_id: str,
    runtime_agent: str,
    registry: dict[str, Any] | None = None,
) -> bool:
    """Validate a private objective-runtime receipt against its exact authority."""

    if not isinstance(receipt, dict):
        return False
    registry = registry or load_agent_contracts_registry()
    try:
        contract = _contract(registry, "objective_runtime_state.v1")
        authority_digest = _objective_runtime_authority_digest(
            session_id=session_id,
            project=project,
            agent_id=agent_id,
            runtime_agent=runtime_agent,
        )
    except (OSError, ValueError, KeyError, TypeError, UnicodeEncodeError):
        return False
    expected_keys = {
        "kind",
        "contract_id",
        "registry_id",
        "registry_version",
        "contract_version",
        "actual_json_bytes",
        "runtime_agent",
        "contract_valid",
        "authority_bound",
        "authority_digest",
        "canonical_digest",
    }
    return bool(
        set(receipt) == expected_keys
        and receipt.get("kind") == OBJECTIVE_RUNTIME_ATTESTATION_KIND
        and receipt.get("contract_id") == "objective_runtime_state.v1"
        and receipt.get("registry_id") == registry.get("registry_id")
        and _exact_contract_integer(receipt.get("registry_version"), registry.get("registry_version"))
        and _exact_contract_integer(receipt.get("contract_version"), contract.get("contract_version"))
        and isinstance(receipt.get("actual_json_bytes"), int)
        and not isinstance(receipt.get("actual_json_bytes"), bool)
        and 0 < receipt["actual_json_bytes"] <= int(contract.get("max_total_json_bytes") or 0)
        and receipt.get("runtime_agent") == runtime_agent
        and receipt.get("contract_valid") is True
        and receipt.get("authority_bound") is True
        and receipt.get("authority_digest") == authority_digest
        and isinstance(receipt.get("canonical_digest"), str)
        and re.fullmatch(r"sha256:[0-9a-f]{64}", receipt["canonical_digest"]) is not None
    )


def _public_trace_identity(value: Any) -> tuple[str, bool]:
    if value in (None, ""):
        return "", False
    if not isinstance(value, str):
        return "", True
    if not re.fullmatch(r"rdt_[0-9a-f]{24}", value) or len(value.encode("utf-8")) > MAX_PUBLIC_TRACE_ID_BYTES:
        return "", True
    return value, False


def _retrieval_receipt_id(value: Any, prefix: str) -> bool:
    return isinstance(value, str) and re.fullmatch(re.escape(prefix) + r"[0-9a-f]{24}", value) is not None


def retrieval_proof_pair_valid(assessment: dict[str, Any], trace: dict[str, Any]) -> bool:
    assessment_unavailable = assessment.get("available") is False
    trace_unavailable = trace.get("available") is False
    if assessment_unavailable or trace_unavailable:
        return assessment_unavailable and trace_unavailable
    summary_fields = (
        (assessment, "assessed_count"),
        (assessment, "quarantine_count"),
        (assessment, "deduplicated_count"),
        (assessment, "policy_omitted_count"),
        (assessment, "input_truncated_count"),
        (trace, "candidate_count"),
        (trace, "decision_count"),
        (trace, "input_truncated_count"),
    )
    if not all(
        isinstance(proof.get(field), int)
        and not isinstance(proof.get(field), bool)
        and 0 <= proof[field] <= MAX_AGENT_CONTRACT_INT
        for proof, field in summary_fields
    ) or not isinstance(trace.get("coverage_complete"), bool):
        return False
    assessed_count = assessment["assessed_count"]
    trace_truncated = trace["input_truncated_count"]
    if (
        assessed_count != trace["decision_count"]
        or assessment["input_truncated_count"] != trace_truncated
        or trace["candidate_count"] != trace["decision_count"] + trace_truncated
        or trace["coverage_complete"] is not (trace_truncated == 0)
        or assessment["quarantine_count"]
        + assessment["deduplicated_count"]
        + assessment["policy_omitted_count"]
        > assessed_count
    ):
        return False
    assessments = assessment.get("assessments")
    decisions = trace.get("decisions")
    if not isinstance(assessments, list) or not isinstance(decisions, list):
        return True
    if (
        assessment.get("input_candidate_count") != trace.get("candidate_count")
        or assessment.get("processed_candidate_count") != trace.get("processed_candidate_count")
        or assessment.get("input_truncated_count") != trace.get("input_truncated_count")
        or trace.get("decision_count") != assessment.get("processed_candidate_count")
    ):
        return False
    assessment_candidates: dict[str, int] = {}
    quarantined_candidates: dict[str, int] = {}
    for row in assessments:
        candidate_id = row.get("candidate_id") if isinstance(row, dict) else None
        quarantine = row.get("quarantine") if isinstance(row, dict) else None
        if not isinstance(candidate_id, str) or not isinstance(quarantine, dict):
            return False
        assessment_candidates[candidate_id] = assessment_candidates.get(candidate_id, 0) + 1
        if quarantine.get("quarantined") is True:
            quarantined_candidates[candidate_id] = quarantined_candidates.get(candidate_id, 0) + 1
    decision_candidates: dict[str, int] = {}
    trace_quarantined_candidates: dict[str, int] = {}
    for row_index, row in enumerate(decisions):
        candidate_id = row.get("candidate_id") if isinstance(row, dict) else None
        decision = row.get("decision") if isinstance(row, dict) else None
        if not isinstance(candidate_id, str) or not isinstance(decision, str):
            return False
        decision_candidates[candidate_id] = decision_candidates.get(candidate_id, 0) + 1
        if decision == "quarantined":
            trace_quarantined_candidates[candidate_id] = trace_quarantined_candidates.get(candidate_id, 0) + 1
    decision_counts = trace.get("decision_counts")
    if not isinstance(decision_counts, dict):
        return False
    broader_omitted = sum(decision_counts.get(category, 0) for category in ("omitted", "omitted_truncated"))
    return (
        assessment_candidates == decision_candidates
        and quarantined_candidates == trace_quarantined_candidates
        and decision_counts.get("quarantined", 0) == assessment.get("quarantine_count")
        and decision_counts.get("deduplicated", 0) == assessment.get("deduplicated_count")
        and broader_omitted >= assessment.get("policy_omitted_count", 0)
    )


def _retrieval_proof_counts_valid(proof: dict[str, Any], kind: str) -> bool:
    fields = (
        (
            "version",
            "input_candidate_count",
            "processed_candidate_count",
            "input_truncated_count",
            "assessed_count",
            "quarantine_count",
            "deduplicated_count",
            "policy_omitted_count",
        )
        if kind == "memory_trust_assessment"
        else (
            "version",
            "candidate_count",
            "processed_candidate_count",
            "input_truncated_count",
            "decision_count",
        )
    )
    if proof.get("version") != 1 or not all(
        isinstance(proof.get(field), int)
        and not isinstance(proof.get(field), bool)
        and 0 <= proof[field] <= MAX_AGENT_CONTRACT_INT
        for field in fields
    ):
        return False
    boundary = proof.get("input_boundary")
    if not isinstance(boundary, dict):
        return False
    maximum_candidates = boundary.get("maximum_candidates")
    omitted_count = boundary.get("omitted_count")
    if (
        isinstance(maximum_candidates, bool)
        or not isinstance(maximum_candidates, int)
        or not 0 <= maximum_candidates <= MAX_AGENT_CONTRACT_INT
        or isinstance(omitted_count, bool)
        or not isinstance(omitted_count, int)
        or not 0 <= omitted_count <= MAX_AGENT_CONTRACT_INT
    ):
        return False
    if kind == "memory_trust_assessment":
        input_count = proof["input_candidate_count"]
        processed = proof["processed_candidate_count"]
        truncated = proof["input_truncated_count"]
        assessed = proof["assessed_count"]
        assessments = proof.get("assessments")
        dispositions = (
            proof["quarantine_count"],
            proof["deduplicated_count"],
            proof["policy_omitted_count"],
        )
        policy = proof.get("policy")
        observed_quarantined = 0
        assessment_ids: set[str] = set()
        candidate_ids: set[str] = set()
        for row in assessments if isinstance(assessments, list) else []:
            if not isinstance(row, dict):
                return False
            quarantine = row.get("quarantine")
            if (
                not _retrieval_receipt_id(row.get("assessment_id"), "mta_")
                or not _retrieval_receipt_id(row.get("candidate_id"), "rtc_")
                or not isinstance(row.get("content_digest"), str)
                or re.fullmatch(r"sha256:[0-9a-f]{64}", row["content_digest"]) is None
                or not isinstance(quarantine, dict)
                or not isinstance(quarantine.get("quarantined"), bool)
            ):
                return False
            assessment_id = row["assessment_id"]
            candidate_id = row["candidate_id"]
            if assessment_id in assessment_ids or candidate_id in candidate_ids:
                return False
            assessment_ids.add(assessment_id)
            candidate_ids.add(candidate_id)
            observed_quarantined += int(quarantine["quarantined"])
        return (
            processed + truncated == input_count
            and assessed == processed
            and isinstance(assessments, list)
            and len(assessments) == assessed
            and maximum_candidates >= processed
            and omitted_count == truncated
            and boundary.get("truncated") is (truncated > 0)
            and isinstance(policy, dict)
            and policy.get("retrieved_memory_is_evidence_not_instruction") is True
            and policy.get("self_awarded_trust_accepted") is False
            and policy.get("security_defenses_fail_closed") is True
            and all(count <= assessed for count in dispositions)
            and sum(dispositions) <= assessed
            and observed_quarantined == proof["quarantine_count"]
        )
    candidate_count = proof["candidate_count"]
    processed = proof["processed_candidate_count"]
    truncated = proof["input_truncated_count"]
    decision_count = proof["decision_count"]
    decisions = proof.get("decisions")
    decision_counts = proof.get("decision_counts")
    if not isinstance(decisions, list) or len(decisions) != decision_count or not isinstance(decision_counts, dict):
        return False
    allowed_decisions = {
        "quarantined",
        "deduplicated",
        "omitted",
        "selected",
        "selected_truncated",
        "omitted_truncated",
    }
    observed_counts: dict[str, int] = {}
    receipt_ids: set[str] = set()
    candidate_ids: set[str] = set()
    candidate_ordinals: set[int] = set()
    for row_index, row in enumerate(decisions):
        if not isinstance(row, dict):
            return False
        decision = row.get("decision")
        ordinal = row.get("candidate_ordinal")
        order = row.get("decision_order")
        if (
            not isinstance(decision, str)
            or decision not in allowed_decisions
            or not _retrieval_receipt_id(row.get("receipt_id"), "rdr_")
            or not _retrieval_receipt_id(row.get("candidate_id"), "rtc_")
            or isinstance(ordinal, bool)
            or not isinstance(ordinal, int)
            or not 1 <= ordinal <= processed
            or isinstance(order, bool)
            or not isinstance(order, int)
            or order != row_index + 1
        ):
            return False
        receipt_id = row["receipt_id"]
        candidate_id = row["candidate_id"]
        if receipt_id in receipt_ids or candidate_id in candidate_ids or ordinal in candidate_ordinals:
            return False
        receipt_ids.add(receipt_id)
        candidate_ids.add(candidate_id)
        candidate_ordinals.add(ordinal)
        observed_counts[decision] = observed_counts.get(decision, 0) + 1
    for category, count in decision_counts.items():
        if (
            not isinstance(category, str)
            or category not in allowed_decisions
            or isinstance(count, bool)
            or not isinstance(count, int)
            or not 0 <= count <= MAX_AGENT_CONTRACT_INT
        ):
            return False
    expected_complete = truncated == 0
    return (
        processed + truncated == candidate_count
        and decision_count == processed
        and maximum_candidates >= processed
        and omitted_count == truncated
        and boundary.get("truncated") is (truncated > 0)
        and decision_counts == observed_counts
        and proof.get("coverage_complete") is expected_complete
    )


def _memory_trust_assessment_reference(assessment: dict[str, Any]) -> dict[str, Any]:
    return {
        "schema_id": "memory_trust_assessment.v1",
        "canonical_path": "$.memory_trust_assessment",
        "assessed_count": _retrieval_proof_count(assessment.get("assessed_count")),
        "quarantine_count": _retrieval_proof_count(assessment.get("quarantine_count")),
        "deduplicated_count": _retrieval_proof_count(assessment.get("deduplicated_count")),
        "policy_omitted_count": _retrieval_proof_count(assessment.get("policy_omitted_count")),
        "input_truncated_count": _retrieval_proof_count(assessment.get("input_truncated_count")),
    }


def _retrieval_decision_trace_reference(trace: dict[str, Any]) -> dict[str, Any]:
    trace_id, _ = _public_trace_identity(trace.get("trace_id"))
    return {
        "schema_id": "retrieval_decision_trace.v1",
        "canonical_path": "$.retrieval_decision_trace",
        "trace_id": trace_id,
        "candidate_count": _retrieval_proof_count(trace.get("candidate_count")),
        "decision_count": _retrieval_proof_count(trace.get("decision_count")),
        "input_truncated_count": _retrieval_proof_count(trace.get("input_truncated_count")),
        "coverage_complete": bool(trace.get("coverage_complete")),
    }


def _canonical_retrieval_proof(value: Any, kind: str, *, allow_reference: bool = False) -> dict[str, Any]:
    proof = dict(value) if isinstance(value, dict) else {}
    expected_schema = "memory_trust_assessment.v1" if kind == "memory_trust_assessment" else "retrieval_decision_trace.v1"
    receipt_list = "assessments" if kind == "memory_trust_assessment" else "decisions"
    if proof.get("schema_id") != expected_schema:
        return {}
    if receipt_list in proof:
        if not agent_contract_json_domain_valid(proof):
            return {}
        try:
            registry = load_agent_contracts_registry()
        except (OSError, ValueError, KeyError, json.JSONDecodeError):
            return {}
        if (
            isinstance(proof.get(receipt_list), list)
            and proof.get("ok") is True
            and proof.get("bounded") is True
            and proof.get("assessed_count" if kind == "memory_trust_assessment" else "decision_count") == len(proof[receipt_list])
            and agent_contract_envelope_attestation_valid(expected_schema, proof, registry)
            and not validate_agent_contract_payload(expected_schema, proof)
            and _retrieval_proof_counts_valid(proof, kind)
        ):
            return proof
        return {}
    if proof.get("available") is False and proof.get("canonical_path") == f"$.{kind}":
        return {
            "schema_id": expected_schema,
            "canonical_path": f"$.{kind}",
            "available": False,
            "reason": "retrieval proof was unavailable at this boundary",
        }
    digest = str(proof.get("canonical_digest") or "")
    if proof.get("bounded_projection") is True and proof.get("canonical_path") == f"$.{kind}" and len(digest) == 71 and digest.startswith("sha256:") and digest == digest.lower():
        try:
            int(digest[7:], 16)
        except ValueError:
            return {}
        count_fields = (
            ("assessed_count", "quarantine_count", "deduplicated_count", "policy_omitted_count", "input_truncated_count")
            if kind == "memory_trust_assessment"
            else ("candidate_count", "decision_count", "input_truncated_count")
        )
        expected_fields = {
            "schema_id",
            "canonical_path",
            "available",
            "bounded_projection",
            "canonical_digest",
            *count_fields,
        }
        if proof.get("available") is not True or not all(
            isinstance(proof.get(field), int)
            and not isinstance(proof.get(field), bool)
            and 0 <= proof[field] <= MAX_AGENT_CONTRACT_INT
            for field in count_fields
        ):
            return {}
        if kind == "retrieval_decision_trace":
            expected_fields.update(("trace_id", "coverage_complete"))
            trace_id = proof.get("trace_id")
            if not isinstance(proof.get("coverage_complete"), bool) or not isinstance(trace_id, str):
                return {}
            if proof.get("trace_id_omitted") is True:
                expected_fields.add("trace_id_omitted")
                if trace_id:
                    return {}
            elif _public_trace_identity(trace_id) != (trace_id, False):
                return {}
        if set(proof) == expected_fields:
            return proof
        return {}
    if allow_reference and proof.get("canonical_path") == f"$.{kind}":
        reference_fields = (
            ("assessed_count", "quarantine_count", "deduplicated_count", "policy_omitted_count", "input_truncated_count")
            if kind == "memory_trust_assessment"
            else ("candidate_count", "decision_count", "input_truncated_count")
        )
        if not all(
            isinstance(proof.get(field), int)
            and not isinstance(proof.get(field), bool)
            and 0 <= proof[field] <= MAX_AGENT_CONTRACT_INT
            for field in reference_fields
        ):
            return {}
        if kind == "retrieval_decision_trace":
            trace_id = str(proof.get("trace_id") or "")
            if _public_trace_identity(trace_id) != (trace_id, False):
                return {}
        expected = (
            _memory_trust_assessment_reference(proof)
            if kind == "memory_trust_assessment"
            else _retrieval_decision_trace_reference(proof)
        )
        if proof == expected:
            return proof
    return {}


def _project_retrieval_proof_before_context_boundary(proof: dict[str, Any], kind: str) -> dict[str, Any]:
    if not proof or proof.get("bounded_projection") is True:
        return proof
    receipt_list = "assessments" if kind == "memory_trust_assessment" else "decisions"
    if receipt_list not in proof:
        return proof
    if not _canonical_retrieval_proof(proof, kind):
        return {
            "schema_id": f"{kind}.v1",
            "canonical_path": f"$.{kind}",
            "available": False,
            "reason": "retrieval proof failed validation before the outer boundary",
        }
    try:
        canonical = _canonical_retrieval_proof_json(proof)
    except (TypeError, ValueError, UnicodeEncodeError, RecursionError):
        return {
            "schema_id": f"{kind}.v1",
            "canonical_path": f"$.{kind}",
            "available": False,
            "reason": "retrieval proof could not be encoded before the outer boundary",
        }
    count_fields = (
        ("assessed_count", "quarantine_count", "deduplicated_count", "policy_omitted_count", "input_truncated_count")
        if kind == "memory_trust_assessment"
        else ("candidate_count", "decision_count", "input_truncated_count")
    )
    projected: dict[str, Any] = {
        "schema_id": f"{kind}.v1",
        "canonical_path": f"$.{kind}",
        "available": True,
        "bounded_projection": True,
        "canonical_digest": "sha256:" + hashlib.sha256(canonical).hexdigest(),
    }
    for field in count_fields:
        projected[field] = proof[field]
    if kind == "retrieval_decision_trace":
        trace_id, trace_id_omitted = _public_trace_identity(proof.get("trace_id"))
        if trace_id and not trace_id_omitted:
            projected["trace_id"] = trace_id
        else:
            projected["trace_id"] = ""
            projected["trace_id_omitted"] = True
        projected["coverage_complete"] = proof.get("coverage_complete") is True
    return projected


def _ensure_context_pack_retrieval_proof_references(payload: dict[str, Any]) -> None:
    if not isinstance(payload, dict):
        return
    pack = dict(payload.get("context_pack")) if isinstance(payload.get("context_pack"), dict) else {}
    compiler = dict(payload.get("context_compiler")) if isinstance(payload.get("context_compiler"), dict) else {}
    if not compiler and isinstance(pack.get("context_compiler"), dict):
        compiler = dict(pack["context_compiler"])

    assessment: dict[str, Any] = {}
    trace: dict[str, Any] = {}
    selected_origin = False
    for owner, allow_reference in ((payload, True), (pack, False), (compiler, False)):
        assessment_present = "memory_trust_assessment" in owner
        trace_present = "retrieval_decision_trace" in owner
        if not assessment_present and not trace_present:
            continue
        selected_origin = True
        candidate_assessment = _canonical_retrieval_proof(
            owner.get("memory_trust_assessment"),
            "memory_trust_assessment",
            allow_reference=allow_reference,
        )
        candidate_trace = _canonical_retrieval_proof(
            owner.get("retrieval_decision_trace"),
            "retrieval_decision_trace",
            allow_reference=allow_reference,
        )
        if candidate_assessment and candidate_trace and retrieval_proof_pair_valid(
            candidate_assessment, candidate_trace
        ):
            assessment, trace = candidate_assessment, candidate_trace
        break
    if not selected_origin or not assessment or not trace:
        assessment = {
            "schema_id": "memory_trust_assessment.v1",
            "canonical_path": "$.memory_trust_assessment",
            "available": False,
            "reason": "a same-origin retrieval proof pair was not available before the outer boundary",
        }
        trace = {
            "schema_id": "retrieval_decision_trace.v1",
            "canonical_path": "$.retrieval_decision_trace",
            "available": False,
            "reason": "a same-origin retrieval proof pair was not available before the outer boundary",
        }
    # Project the untouched authoritative receipts only after their shared
    # candidate/count/disposition spine has reconciled. Digests must bind the
    # producer receipts, not path-stamped or independently clipped derivatives.
    assessment = _project_retrieval_proof_before_context_boundary(dict(assessment), "memory_trust_assessment")
    assessment.setdefault("schema_id", "memory_trust_assessment.v1")
    assessment.setdefault("canonical_path", "$.memory_trust_assessment")
    trace = _project_retrieval_proof_before_context_boundary(dict(trace), "retrieval_decision_trace")
    trace.setdefault("schema_id", "retrieval_decision_trace.v1")
    trace.setdefault("canonical_path", "$.retrieval_decision_trace")

    assessment_reference = (
        dict(assessment)
        if assessment.get("available") is False or assessment.get("bounded_projection") is True
        else _memory_trust_assessment_reference(assessment)
    )
    trace_reference = (
        dict(trace)
        if trace.get("available") is False or trace.get("bounded_projection") is True
        else _retrieval_decision_trace_reference(trace)
    )
    payload["memory_trust_assessment"] = assessment
    payload["retrieval_decision_trace"] = trace
    if pack:
        pack["memory_trust_assessment"] = assessment_reference
        pack["retrieval_decision_trace"] = trace_reference
        payload["context_pack"] = pack
    if compiler:
        compiler["memory_trust_assessment"] = assessment_reference
        compiler["retrieval_decision_trace"] = trace_reference
        payload["context_compiler"] = compiler
        if pack:
            pack["context_compiler"] = compiler


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
        _ensure_context_pack_retrieval_proof_references(payload)
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
    if not agent_contract_json_domain_valid(payload):
        findings.append({"reason": "payload_json_domain_invalid", "contract_id": contract_id})

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
        exists, value = _path_lookup(payload, str(dotted_path))
        if not exists:
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

    closed_fields = contract.get("closed_fields_by_path")
    if isinstance(closed_fields, dict):
        for dotted_path, fields in closed_fields.items():
            target = _path_get(payload, str(dotted_path))
            if target is None:
                continue
            if not isinstance(target, dict):
                findings.append({"reason": "closed_path_not_object", "path": str(dotted_path), "contract_id": contract_id})
                continue
            allowed_nested = {str(field) for field in fields or []}
            for field in sorted(set(target) - allowed_nested):
                findings.append(
                    {
                        "reason": "unexpected_nested_field",
                        "path": f"{dotted_path}.{field}",
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

    state_matrix = contract.get("state_matrix")
    if isinstance(state_matrix, list) and state_matrix:
        matched = False
        for row in state_matrix:
            if not isinstance(row, dict) or not row:
                continue
            if all(_path_get(payload, str(path)) == expected for path, expected in row.items()):
                matched = True
                break
        if not matched:
            findings.append({"reason": "state_matrix_mismatch", "path": "state_matrix", "contract_id": contract_id})

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
            actual = len(value.encode("utf-8", errors="replace"))
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

    forbidden = {
        _canonical_agent_contract_field_key(item)
        for item in contract.get("forbidden_fields") or []
        if str(item)
    }
    if forbidden:
        if str(contract.get("forbidden_scope") or "recursive") == "root":
            for key in sorted(payload):
                if _canonical_agent_contract_field_key(key) in forbidden:
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
    if contract_id in {
        "universal_agent_adapter_response.v1",
        "contextlattice_lifecycle_receipt.v1",
    }:
        identity_fields = (
            ("session_id", "agent_id")
            if contract_id == "universal_agent_adapter_response.v1"
            else ("session_id",)
        )
        raw_omitted = payload.get("identity_omitted")
        omitted = set(raw_omitted) if isinstance(raw_omitted, list) and all(isinstance(item, str) for item in raw_omitted) else set()
        if raw_omitted is not None and (
            not isinstance(raw_omitted, list)
            or not all(isinstance(item, str) for item in raw_omitted)
            or not omitted.issubset(set(identity_fields))
        ):
            findings.append(
                {"reason": "identity_omission_marker_invalid", "path": "identity_omitted", "contract_id": contract_id}
            )
        for identity_field in identity_fields:
            value = payload.get(identity_field)
            present = isinstance(value, str) and bool(value.strip())
            marked = identity_field in omitted
            if present == marked:
                findings.append(
                    {
                        "reason": "identity_or_omission_required" if not present else "identity_omission_conflict",
                        "path": identity_field,
                        "contract_id": contract_id,
                    }
                )
    if contract_id in {"memory_trust_assessment.v1", "retrieval_decision_trace.v1"}:
        kind = (
            "memory_trust_assessment"
            if contract_id == "memory_trust_assessment.v1"
            else "retrieval_decision_trace"
        )
        if not _retrieval_proof_counts_valid(payload, kind):
            findings.append(
                {
                    "reason": "retrieval_proof_count_invariant_mismatch",
                    "path": "retrieval_counts",
                    "contract_id": contract_id,
                }
            )
    return findings


def validate_agent_task_publication_reconciliation(
    payload: Any,
    registry: dict[str, Any] | None = None,
) -> list[dict[str, Any]]:
    """Validate the exact cross-language U3 restart publication boundary."""

    contract_id = "agent_task_publication_reconciliation.v1"
    registry = registry or load_agent_contracts_registry()
    findings = validate_agent_contract_payload(contract_id, payload, registry)
    if not isinstance(payload, dict):
        return findings

    def finding(reason: str, path: str) -> None:
        findings.append({"reason": reason, "path": path, "contract_id": contract_id})

    if payload.get("schema_id") != contract_id:
        finding("schema_id_mismatch", "schema_id")
    for identity_field in (
        "publication_id",
        "result_id",
        "task_id",
        "attempt_id",
        "lease_id",
        "worker_id",
        "worker_instance_id",
    ):
        if not str(payload.get(identity_field) or "").strip():
            finding("identity_field_missing", identity_field)
    idempotency_key = str(payload.get("idempotency_key") or "")
    if (
        not idempotency_key.strip()
        or idempotency_key != idempotency_key.strip()
        or len(idempotency_key.encode("utf-8")) > 2048
    ):
        finding("idempotency_key_mismatch", "idempotency_key")
    generations = (payload.get("generation"), payload.get("assignment_generation"), payload.get("lease_generation"))
    if not isinstance(generations[0], int) or isinstance(generations[0], bool) or generations[0] <= 0 or len(set(generations)) != 1:
        finding("generation_fence_mismatch", "generation")

    receipt = payload.get("publication_receipt")
    authorization = payload.get("cleanup_authorization")
    if not isinstance(receipt, dict) or not isinstance(authorization, dict):
        return findings
    expected_identity = {
        "publication_id": payload.get("publication_id"),
        "result_id": payload.get("result_id"),
        "task_id": payload.get("task_id"),
        "attempt_id": payload.get("attempt_id"),
        "lease_id": payload.get("lease_id"),
        "generation": payload.get("lease_generation"),
        "worker_id": payload.get("worker_id"),
        "worker_instance_id": payload.get("worker_instance_id"),
    }
    for path, expected in expected_identity.items():
        if receipt.get(path) != expected:
            finding("publication_receipt_fence_mismatch", f"publication_receipt.{path}")
        if authorization.get(path) != expected:
            finding("cleanup_authorization_fence_mismatch", f"cleanup_authorization.{path}")
    if receipt.get("schema_id") != "agent_task_publication_receipt.v1" or receipt.get("authority") != "gateway-go-sqlite-wal" or receipt.get("durable") is not True or receipt.get("state") != "staged":
        finding("publication_receipt_contract_mismatch", "publication_receipt")
    if not str(receipt.get("receipt_id") or "").strip():
        finding("publication_receipt_identity_missing", "publication_receipt.receipt_id")
    if authorization.get("schema_id") != "agent_task_cleanup_authorization.v1" or authorization.get("authority") != "gateway-go-sqlite-wal" or authorization.get("authorized") is not True or authorization.get("attempt_terminal") is not True or authorization.get("durable") is not True or authorization.get("state") != "authorized":
        finding("cleanup_authorization_contract_mismatch", "cleanup_authorization")
    if not str(authorization.get("authorization_id") or "").strip():
        finding("cleanup_authorization_identity_missing", "cleanup_authorization.authorization_id")

    def canonical_digest(value: dict[str, Any], digest_field: str) -> str:
        material = {str(key): nested for key, nested in value.items() if str(key) != digest_field}
        encoded = json.dumps(material, ensure_ascii=True, sort_keys=True, separators=(",", ":")).encode("utf-8")
        return "sha256:" + hashlib.sha256(encoded).hexdigest()

    if receipt.get("receipt_digest") != canonical_digest(receipt, "receipt_digest"):
        finding("publication_receipt_digest_mismatch", "publication_receipt.receipt_digest")
    if authorization.get("authorization_digest") != canonical_digest(authorization, "authorization_digest"):
        finding("cleanup_authorization_digest_mismatch", "cleanup_authorization.authorization_digest")
    workspace_ref = str(authorization.get("workspace_ref") or "")
    cleanup_material = f"{payload.get('task_id') or ''}\0{payload.get('attempt_id') or ''}\0{workspace_ref}".encode("utf-8")
    expected_cleanup_id = "cleanup-" + hashlib.sha256(cleanup_material).hexdigest()[:32]
    if not workspace_ref or authorization.get("cleanup_id") != expected_cleanup_id:
        finding("cleanup_authorization_workspace_mismatch", "cleanup_authorization.cleanup_id")
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


def _stabilize_contract_actual_json_bytes(payload: dict[str, Any], metadata_key: str) -> None:
    metadata = payload.get(metadata_key)
    if not isinstance(metadata, dict):
        return
    for _ in range(12):
        actual = _json_bytes(payload)
        if actual < 0 or metadata.get("actual_json_bytes") == actual:
            return
        metadata["actual_json_bytes"] = actual


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
    input_domain_valid = agent_contract_json_domain_valid(payload)
    stamped = dict(payload) if input_domain_valid else {"ok": False}
    if contract_id == "context_pack_response.v1":
        _ensure_context_pack_retrieval_proof_references(stamped)
        _ensure_context_pack_run_advisor(stamped, registry)
    metadata = contract_metadata(contract_id, registry)
    previous_metadata = stamped.get("format_contract") if isinstance(stamped.get("format_contract"), dict) else {}
    stamped["format_contract"] = metadata
    previous_counts: dict[str, Any] | None = previous_metadata.get("omitted_counts") if isinstance(previous_metadata, dict) else None
    domain_findings: list[dict[str, Any]] = []
    if not input_domain_valid:
        domain_findings.append({"reason": "payload_json_domain_invalid", "contract_id": contract_id})
    findings: list[dict[str, Any]] = list(domain_findings)
    before = _json_bytes(stamped)
    after = before
    for _ in range(5):
        before = _json_bytes(stamped)
        stamped = enforce_contract_limits(contract_id, stamped, registry)
        after = _json_bytes(stamped)
        findings = domain_findings + validate_agent_contract_payload(contract_id, stamped, registry)
        stamped["format_contract"] = stamp_validation(metadata, findings, stamped, before, after, previous_counts)
        previous_counts = stamped["format_contract"].get("omitted_counts") if isinstance(stamped.get("format_contract"), dict) else previous_counts
        post_stamp_findings = domain_findings + validate_agent_contract_payload(contract_id, stamped, registry)
        if not post_stamp_findings:
            stamped["format_contract"] = stamp_validation(metadata, post_stamp_findings, stamped, before, after, previous_counts)
            previous_counts = stamped["format_contract"].get("omitted_counts") if isinstance(stamped.get("format_contract"), dict) else previous_counts
            findings = domain_findings + validate_agent_contract_payload(contract_id, stamped, registry)
            if findings:
                continue
            break
        findings = post_stamp_findings
    if findings:
        stamped["format_contract"] = stamp_validation(metadata, findings, stamped, before, after, previous_counts)
    _stabilize_contract_actual_json_bytes(stamped, "format_contract")
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
    available_contracts = registry.get("contracts") if isinstance(registry.get("contracts"), dict) else {}
    relevant_contracts = [
        contract_id
        for contract_id in (
            "agent_preflight_response.v1",
            "policy_context_package.v1",
            "objective_runtime_state.v1",
            "anti_scheming_protocol.v1",
            "context_pack_response.v1",
            "dream_mode_response.v1",
            "review_mode_response.v1",
            "writeback_result.v1",
            "codex_compact_hook_stdout.v1",
            "agent_task_result.v1",
            "contract_acknowledgement.v1",
            "agent_span.v1",
            "agent_flight_recorder_event.v1",
            "a2a_readiness_profile.v1",
            "agent_session_rollup.v1",
            "agent_prompt_context_package.v1",
            "agent_run_trace.v1",
            "agent_proof_timeline.v1",
            "continuous_cognition.v1",
            "run_advisor.v1",
            "retrieval_progress.v1",
            "steering_comment.v1",
        )
        if contract_id in available_contracts
    ]
    observed_counts = _boundary_omitted_counts(payload) if isinstance(payload, dict) else _empty_boundary_counts()
    if original_json_bytes > 0 and bounded_json_bytes > 0 and original_json_bytes > bounded_json_bytes:
        observed_counts["json_bytes_reduced"] += original_json_bytes - bounded_json_bytes
        observed_counts["boundary_passes"] += 1
    counts = _merge_boundary_counts(previous_counts, observed_counts)
    truncated = any(value > 0 for value in counts.values())
    summary = {
        "registry_id": str(registry.get("registry_id") or "contextlattice_agent_output_contracts"),
        "registry_version": int(registry.get("registry_version") or 0),
        "contracts": relevant_contracts,
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
    _stabilize_contract_actual_json_bytes(response, "format_contracts")
    return response
