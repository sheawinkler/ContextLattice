from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from exact_state import (
    EXACT_STATE_INDEX_SCHEMA_ID,
    ExactStateRegistryError,
    is_exact_state_path,
    load_exact_state_paths,
    parse_exact_state_paths,
)


class ExactStateRegistryTests(unittest.TestCase):
    def test_registry_normalizes_and_matches_canonical_variants(self) -> None:
        paths = parse_exact_state_paths(
            '{"schema_id":"contextlattice_exact_state_index.v1",'
            '"paths":["Project::Runtime/State.JSON"]}'
        )
        self.assertEqual(paths, {"project::runtime/state.json"})
        self.assertTrue(is_exact_state_path(paths, "PROJECT", "runtime\\state.json"))
        self.assertTrue(is_exact_state_path(paths, "project", "runtime//./state.json"))
        self.assertTrue(is_exact_state_path(paths, "project", " runtime / state.json "))
        self.assertTrue(is_exact_state_path(paths, "project", "runtime/../state.json"))
        self.assertTrue(is_exact_state_path(paths, "project", "runtime::state.json"))
        self.assertTrue(is_exact_state_path(paths, "project::alias", "learning.md"))
        self.assertFalse(is_exact_state_path(paths, "project", "learning.md"))

    def test_registry_rejects_invalid_keys_and_schema(self) -> None:
        with self.assertRaisesRegex(ExactStateRegistryError, "schema mismatch"):
            parse_exact_state_paths('{"schema_id":"other.v1","paths":[]}')
        for invalid in ("project::../state.json", "project::runtime::state.json"):
            with self.subTest(invalid=invalid):
                with self.assertRaisesRegex(ExactStateRegistryError, "invalid path key"):
                    parse_exact_state_paths(
                        '{"schema_id":"contextlattice_exact_state_index.v1",'
                        f'"paths":["{invalid}"]}}'
                    )

    def test_missing_registry_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            missing = Path(tmp) / "missing.json"
            with self.assertRaisesRegex(ExactStateRegistryError, "read exact-state registry"):
                load_exact_state_paths(missing)

    def test_symlinked_registry_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "registry-target.json"
            target.write_text(
                '{"schema_id":"contextlattice_exact_state_index.v1","paths":[]}',
                encoding="utf-8",
            )
            registry = Path(tmp) / "exact-state.json"
            try:
                registry.symlink_to(target)
            except (NotImplementedError, OSError) as exc:
                self.skipTest(f"symlink unavailable: {exc}")
            with self.assertRaisesRegex(ExactStateRegistryError, "must not be a symlink"):
                load_exact_state_paths(registry)

    def test_registry_path_limit_is_bounded(self) -> None:
        payload = (
            '{"schema_id":"'
            + EXACT_STATE_INDEX_SCHEMA_ID
            + '","paths":['
            + ",".join(f'"project::state/{index}.json"' for index in range(100_001))
            + "]}"
        )
        with self.assertRaisesRegex(ExactStateRegistryError, "bounded path limit"):
            parse_exact_state_paths(payload)


if __name__ == "__main__":
    unittest.main()
