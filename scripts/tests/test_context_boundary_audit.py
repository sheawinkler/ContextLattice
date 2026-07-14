from __future__ import annotations

import importlib.machinery
import importlib.util
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
AGENT_SCRIPTS = ROOT / "scripts" / "agent"
sys.path.insert(0, str(AGENT_SCRIPTS))

loader = importlib.machinery.SourceFileLoader("audit_context_boundary", str(AGENT_SCRIPTS / "audit-context-boundary"))
spec = importlib.util.spec_from_loader(loader.name, loader)
assert spec is not None
audit_context_boundary = importlib.util.module_from_spec(spec)
loader.exec_module(audit_context_boundary)


def valid_route(path: str, contract_id: str) -> dict[str, object]:
    return {
        "name": contract_id,
        "path": path,
        "contract_id": contract_id,
        "bounded": True,
        "max_total_json_bytes": 4096,
        "max_string_bytes": 1024,
        "max_list_items": 32,
        "metadata_fields": list(audit_context_boundary.REQUIRED_METADATA_FIELDS),
    }


def valid_payload() -> dict[str, object]:
    routes = [valid_route(path, f"fixture:{path}") for path in audit_context_boundary.REQUIRED_PATHS]
    routes = [route for route in routes if route["path"] != "/memory/decision-changes"]
    contract_routes = [valid_route(path, contract_id) for path, contract_id in audit_context_boundary.REQUIRED_CONTRACT_SURFACES]
    contract_paths = {(path, contract_id) for path, contract_id in audit_context_boundary.REQUIRED_CONTRACT_SURFACES}
    routes = [
        route
        for route in routes
        if (str(route["path"]), str(route["contract_id"])) not in contract_paths
        and str(route["path"]) not in {path for path, _ in contract_paths}
    ]
    routes.extend(contract_routes)
    return {
        "ok": True,
        "schema_id": audit_context_boundary.SCHEMA_ID,
        "status": "bounded",
        "violationCount": 0,
        "routes": routes,
        "forbidden_error_markers": [],
    }


class ContextBoundaryAuditTests(unittest.TestCase):
    def test_duplicate_path_contracts_are_both_required(self) -> None:
        payload = valid_payload()
        result = audit_context_boundary.audit_payload(payload)
        self.assertTrue(result["ok"], result["findings"])

        payload["routes"] = [
            route
            for route in payload["routes"]
            if not (
                route["path"] == "/memory/decision-changes"
                and route["contract_id"] == "decision_change_query.v1"
            )
        ]
        result = audit_context_boundary.audit_payload(payload)
        self.assertFalse(result["ok"])
        self.assertIn(
            {
                "reason": "required_contract_boundary_missing",
                "path": "/memory/decision-changes",
                "contract_id": "decision_change_query.v1",
            },
            result["findings"],
        )

    def test_duplicate_path_unbounded_contract_cannot_hide(self) -> None:
        payload = valid_payload()
        query_route = next(
            route
            for route in payload["routes"]
            if route["path"] == "/memory/decision-changes"
            and route["contract_id"] == "decision_change_query.v1"
        )
        query_route["bounded"] = False
        result = audit_context_boundary.audit_payload(payload)
        self.assertFalse(result["ok"])
        self.assertTrue(
            any(
                finding.get("reason") == "required_boundary_not_bounded"
                and finding.get("contract_id") == "decision_change_query.v1"
                for finding in result["findings"]
            ),
            result["findings"],
        )


if __name__ == "__main__":
    unittest.main()
