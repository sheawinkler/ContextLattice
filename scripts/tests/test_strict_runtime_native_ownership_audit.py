from __future__ import annotations

import importlib.machinery
import importlib.util
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
AGENT_SCRIPTS = ROOT / "scripts" / "agent"
sys.path.insert(0, str(AGENT_SCRIPTS))

loader = importlib.machinery.SourceFileLoader(
    "audit_strict_runtime_native_ownership",
    str(AGENT_SCRIPTS / "audit-strict-runtime-native-ownership"),
)
spec = importlib.util.spec_from_loader(loader.name, loader)
assert spec is not None
audit_native_ownership = importlib.util.module_from_spec(spec)
loader.exec_module(audit_native_ownership)


SOURCE_COMMIT = "a" * 40
SOURCE_TREE = "b" * 40
BOOT_NONCE = "c" * 32


def expected_identity() -> dict[str, str]:
    return {
        "expected_source_commit": SOURCE_COMMIT,
        "expected_source_tree": SOURCE_TREE,
        "expected_boot_nonce": BOOT_NONCE,
    }


def valid_payload() -> dict[str, object]:
    registry = audit_native_ownership.agent_contract_registry_identity()
    return {
        "ok": True,
        "schema_id": audit_native_ownership.SCHEMA_ID,
        "generatedAt": audit_native_ownership.now_iso(),
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
        "status": "clean",
        "violationCount": 0,
        "pythonHotPathOwnership": {"fallbacks": 0, "byPath": {}},
        "routes": [
            {
                "path": path,
                "owner": "go_native",
                "status": "native",
                "strictRuntimeCompatible": True,
            }
            for path in audit_native_ownership.REQUIRED_PATHS
        ],
        "forbidden_error_markers": [],
    }


class StrictRuntimeNativeOwnershipAuditTests(unittest.TestCase):
    def test_expected_runtime_identity_matches(self) -> None:
        result = audit_native_ownership.audit_payload(valid_payload(), **expected_identity())
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
                result = audit_native_ownership.audit_payload(payload, **expected_identity())
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
        result = audit_native_ownership.audit_payload(payload, **expected_identity())
        self.assertFalse(result["ok"])
        self.assertTrue(
            any(finding.get("reason") == "build_identity_missing" for finding in result["findings"]),
            result["findings"],
        )

    def test_expected_boot_nonce_missing_is_rejected(self) -> None:
        payload = valid_payload()
        payload["build"].pop("boot_nonce")
        result = audit_native_ownership.audit_payload(payload, **expected_identity())
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
        result = audit_native_ownership.audit_payload(payload)
        self.assertFalse(result["ok"])
        self.assertTrue(
            any(finding.get("reason") == "generated_at_stale" for finding in result["findings"]),
            result["findings"],
        )

    def test_current_registry_and_t2_routes_pass(self) -> None:
        result = audit_native_ownership.audit_payload(valid_payload())
        self.assertTrue(result["ok"], result["findings"])

    def test_stale_registry_is_rejected(self) -> None:
        payload = valid_payload()
        payload["registry_version"] = int(payload["registry_version"]) - 1
        result = audit_native_ownership.audit_payload(payload)
        self.assertFalse(result["ok"])
        self.assertTrue(
            any(finding.get("reason") == "registry_version_mismatch" for finding in result["findings"]),
            result["findings"],
        )


if __name__ == "__main__":
    unittest.main()
