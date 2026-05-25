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
    return findings


def contract_metadata(contract_id: str, registry: dict[str, Any] | None = None) -> dict[str, Any]:
    registry = registry or load_agent_contracts_registry()
    contract = _contract(registry, contract_id)
    return {
        "registry_id": str(registry.get("registry_id") or "contextlattice_agent_output_contracts"),
        "registry_version": int(registry.get("registry_version") or 0),
        "schema_id": contract_id,
        "contract_version": int(contract.get("contract_version") or 0),
        "required_output_mode": str(contract.get("required_output_mode") or "json_object"),
        "validator": str(registry.get("default_validator") or "contextlattice.boundary.v1"),
        "forbidden_fields": [str(item) for item in contract.get("forbidden_fields") or []],
        "validation": {"status": "pending", "errors": []},
    }


def stamp_validation(metadata: dict[str, Any], findings: list[dict[str, Any]]) -> dict[str, Any]:
    stamped = deepcopy(metadata)
    stamped["validation"] = {
        "status": "failed" if findings else "passed",
        "errors": findings[:12],
    }
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
    metadata = contract_metadata(contract_id, registry)
    stamped["format_contract"] = metadata
    findings = validate_agent_contract_payload(contract_id, stamped, registry)
    stamped["format_contract"] = stamp_validation(metadata, findings)
    return stamped


def preflight_contracts_summary(findings: list[dict[str, Any]] | None = None) -> dict[str, Any]:
    registry = load_agent_contracts_registry()
    errors = findings or []
    return {
        "registry_id": str(registry.get("registry_id") or "contextlattice_agent_output_contracts"),
        "registry_version": int(registry.get("registry_version") or 0),
        "contracts": agent_contract_ids(registry),
        "validation": {
            "status": "failed" if errors else "passed",
            "errors": errors[:12],
        },
    }
