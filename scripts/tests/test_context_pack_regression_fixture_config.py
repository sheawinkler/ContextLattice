from __future__ import annotations

import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


class ContextPackRegressionFixtureConfigTests(unittest.TestCase):
    def test_gateway_compose_passes_fixture_sidecar_configuration(self) -> None:
        compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
        required = (
            "GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_ENABLED: ${GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_ENABLED:-true}",
            "GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH: ${GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH:-}",
            "GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_MAX_BYTES: ${GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_MAX_BYTES:-2097152}",
            "GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_MAX_FIXTURES: ${GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_MAX_FIXTURES:-1000}",
        )
        for expected in required:
            self.assertIn(expected, compose)

    def test_example_preserves_sibling_fallback_and_owner_only_limits(self) -> None:
        env_example = (ROOT / ".env.example").read_text(encoding="utf-8")
        self.assertIn("# sidecar; leave its path blank to place it beside the quality ledger (and", env_example)
        required = (
            "GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_ENABLED=true",
            "GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH=",
            "GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_MAX_BYTES=2097152",
            "GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_MAX_FIXTURES=1000",
        )
        for expected in required:
            self.assertIn(expected, env_example)


if __name__ == "__main__":
    unittest.main()
