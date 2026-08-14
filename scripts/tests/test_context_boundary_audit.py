from __future__ import annotations

import importlib.machinery
import importlib.util
import io
import json
import sys
import unittest
from contextlib import redirect_stdout
from datetime import datetime, timezone
from unittest import mock
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
AGENT_SCRIPTS = ROOT / "scripts" / "agent"
sys.path.insert(0, str(AGENT_SCRIPTS))

loader = importlib.machinery.SourceFileLoader("audit_context_boundary", str(AGENT_SCRIPTS / "audit-context-boundary"))
spec = importlib.util.spec_from_loader(loader.name, loader)
assert spec is not None
audit_context_boundary = importlib.util.module_from_spec(spec)
loader.exec_module(audit_context_boundary)


SOURCE_COMMIT = "a" * 40
SOURCE_TREE = "b" * 40
BOOT_NONCE = "c" * 32


def expected_identity() -> dict[str, str]:
    return {
        "expected_source_commit": SOURCE_COMMIT,
        "expected_source_tree": SOURCE_TREE,
        "expected_boot_nonce": BOOT_NONCE,
    }


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
    registry = audit_context_boundary.agent_contract_registry_identity()
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
        "generatedAt": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "build": {
            "schema_id": "contextlattice_build_identity.v1",
            "version": "3.19.0-rc.1",
            "channel": "candidate",
            "source_commit": SOURCE_COMMIT,
            "source_tree": SOURCE_TREE,
            "source_bound": True,
            "boot_nonce": BOOT_NONCE,
        },
        "registry_id": registry["registry_id"],
        "registry_version": registry["registry_version"],
        "status": "bounded",
        "violationCount": 0,
        "routes": routes,
        "forbidden_error_markers": [],
    }


class ContextBoundaryAuditTests(unittest.TestCase):
    def test_cli_requests_exact_payload_before_safe_emission(self) -> None:
        payload = valid_payload()
        stdout = io.StringIO()
        with mock.patch.object(
            audit_context_boundary, "request_json_for_validation", return_value=payload
        ) as request, mock.patch.object(sys, "argv", ["audit-context-boundary"]), redirect_stdout(stdout):
            self.assertEqual(audit_context_boundary.main(), 0)
        request.assert_called_once_with("GET", "/ops/context-boundary", None, 10)
        emitted = json.loads(stdout.getvalue())
        self.assertTrue(emitted["ok"])
        self.assertEqual(emitted["build"]["source_commit"], "[REDACTED_TOKEN]")

    def test_expected_runtime_identity_matches(self) -> None:
        result = audit_context_boundary.audit_payload(valid_payload(), **expected_identity())
        self.assertTrue(result["ok"], result["findings"])
        self.assertEqual(
            result["expected_build"],
            {"source_commit": SOURCE_COMMIT, "source_tree": SOURCE_TREE, "boot_nonce": BOOT_NONCE},
        )

    def test_expected_runtime_identity_mismatches_are_rejected(self) -> None:
        replacements = {
            "source_commit": "d" * 40,
            "source_tree": "e" * 40,
            "boot_nonce": "f" * 32,
        }
        for field, replacement in replacements.items():
            with self.subTest(field=field):
                payload = valid_payload()
                payload["build"][field] = replacement
                result = audit_context_boundary.audit_payload(payload, **expected_identity())
                self.assertFalse(result["ok"])
                self.assertTrue(
                    any(
                        finding.get("reason") == "build_identity_mismatch" and finding.get("field") == field
                        for finding in result["findings"]
                    ),
                    result["findings"],
                )

    def test_expected_runtime_identity_missing_object_is_rejected(self) -> None:
        payload = valid_payload()
        payload.pop("build")
        result = audit_context_boundary.audit_payload(payload, **expected_identity())
        self.assertFalse(result["ok"])
        self.assertTrue(
            any(finding.get("reason") == "build_identity_missing" for finding in result["findings"]),
            result["findings"],
        )

    def test_expected_boot_nonce_missing_is_rejected(self) -> None:
        payload = valid_payload()
        payload["build"].pop("boot_nonce")
        result = audit_context_boundary.audit_payload(payload, **expected_identity())
        self.assertFalse(result["ok"])
        self.assertTrue(
            any(
                finding.get("reason") == "build_identity_field_missing" and finding.get("field") == "boot_nonce"
                for finding in result["findings"]
            ),
            result["findings"],
        )

    def test_stale_generated_at_is_rejected(self) -> None:
        payload = valid_payload()
        payload["generatedAt"] = "2000-01-01T00:00:00Z"
        result = audit_context_boundary.audit_payload(payload)
        self.assertFalse(result["ok"])
        self.assertTrue(
            any(finding.get("reason") == "generated_at_stale" for finding in result["findings"]),
            result["findings"],
        )

    def test_stale_registry_is_rejected(self) -> None:
        payload = valid_payload()
        payload["registry_version"] = int(payload["registry_version"]) - 1
        result = audit_context_boundary.audit_payload(payload)
        self.assertFalse(result["ok"])
        self.assertTrue(
            any(finding.get("reason") == "registry_version_mismatch" for finding in result["findings"]),
            result["findings"],
        )

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
