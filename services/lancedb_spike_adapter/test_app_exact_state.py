from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import app


class AppExactStateBoundaryTests(unittest.TestCase):
    def test_search_uses_registry_loaded_after_backend_read(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            registry = Path(tmp) / "exact_state_paths.json"
            registry.write_text(
                json.dumps(
                    {
                        "schema_id": "contextlattice_exact_state_index.v1",
                        "paths": [],
                    }
                ),
                encoding="utf-8",
            )

            def backend_search(_query: str, _limit: int) -> list[dict[str, object]]:
                registry.write_text(
                    json.dumps(
                        {
                            "schema_id": "contextlattice_exact_state_index.v1",
                            "paths": ["project::runtime/state.json"],
                        }
                    ),
                    encoding="utf-8",
                )
                return [
                    {
                        "project": "project",
                        "file": "runtime/state.json",
                        "summary": "must stay private",
                        "score": 2.0,
                        "topic_path": "runtime",
                    },
                    {
                        "project": "project",
                        "file": "learning.md",
                        "summary": "durable learning",
                        "score": 1.0,
                        "topic_path": "general",
                    },
                ]

            with (
                patch.object(app, "EXACT_STATE_INDEX_PATH", registry),
                patch.object(app, "_trigger_refresh"),
                patch.object(app, "_lancedb_search", side_effect=backend_search),
            ):
                response = app.search(app.SearchRequest(query="state", limit=10))

            self.assertEqual([row.file for row in response.results], ["learning.md"])

    def test_exact_rows_do_not_consume_result_limit(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            registry = Path(tmp) / "exact_state_paths.json"
            registry.write_text(
                json.dumps(
                    {
                        "schema_id": "contextlattice_exact_state_index.v1",
                        "paths": ["project::runtime/state.json"],
                    }
                ),
                encoding="utf-8",
            )
            rows = [
                {
                    "project": "project",
                    "file": "runtime/state.json",
                    "summary": "must stay private",
                    "score": 2.0,
                    "topic_path": "runtime",
                },
                {
                    "project": "project",
                    "file": "learning.md",
                    "summary": "durable learning",
                    "score": 1.0,
                    "topic_path": "general",
                },
            ]

            def backend_search(_query: str, limit: int) -> list[dict[str, object]]:
                return rows[:limit]

            with (
                patch.object(app, "EXACT_STATE_INDEX_PATH", registry),
                patch.object(app, "_trigger_refresh"),
                patch.object(app, "_lancedb_search", side_effect=backend_search),
            ):
                response = app.search(app.SearchRequest(query="state", limit=1))

            self.assertEqual([row.file for row in response.results], ["learning.md"])

    def test_scan_resolves_symlink_targets_without_breaking_content_blob_links(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "memory-bank"
            registry = root / "_contextlattice" / "exact_state_paths.json"
            registry.parent.mkdir(parents=True)
            registry.write_text(
                json.dumps(
                    {
                        "schema_id": "contextlattice_exact_state_index.v1",
                        "paths": ["project::runtime/state.json"],
                    }
                ),
                encoding="utf-8",
            )
            exact = root / "project" / "runtime" / "state.json"
            exact.parent.mkdir(parents=True)
            exact.write_text("exact secret", encoding="utf-8")
            content_blobs = root / "_contextlattice" / "content_blobs"
            blob = content_blobs / "aa" / ("a" * 64 + ".txt")
            blob.parent.mkdir(parents=True)
            blob.write_text("durable blob learning", encoding="utf-8")
            notes = root / "project" / "notes"
            notes.mkdir(parents=True)
            exact_alias = notes / "exact-alias.md"
            blob_alias = notes / "blob-alias.md"
            other_project = root / "other-project" / "private.md"
            other_project.parent.mkdir(parents=True)
            other_project.write_text("other project private context", encoding="utf-8")
            cross_project_alias = notes / "cross-project-alias.md"
            try:
                exact_alias.symlink_to(exact)
                blob_alias.symlink_to(blob)
                cross_project_alias.symlink_to(other_project)
            except (NotImplementedError, OSError) as exc:
                self.skipTest(f"symlink unavailable: {exc}")

            with (
                patch.object(app, "DATA_ROOT", root),
                patch.object(app, "EXACT_STATE_INDEX_PATH", registry),
                patch.object(app, "CONTENT_BLOBS_PATH", content_blobs),
            ):
                docs, _ = app._scan_docs()

            files = {str(row["file"]) for row in docs}
            self.assertNotIn("notes/exact-alias.md", files)
            self.assertNotIn("notes/cross-project-alias.md", files)
            self.assertIn("notes/blob-alias.md", files)

    def test_scan_rejects_symlinked_data_root(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "target"
            registry = target / "_contextlattice" / "exact_state_paths.json"
            registry.parent.mkdir(parents=True)
            registry.write_text(
                json.dumps(
                    {
                        "schema_id": "contextlattice_exact_state_index.v1",
                        "paths": [],
                    }
                ),
                encoding="utf-8",
            )
            root = Path(tmp) / "memory-bank"
            try:
                root.symlink_to(target, target_is_directory=True)
            except (NotImplementedError, OSError) as exc:
                self.skipTest(f"symlink unavailable: {exc}")
            with (
                patch.object(app, "DATA_ROOT", root),
                patch.object(app, "EXACT_STATE_INDEX_PATH", registry),
            ):
                with self.assertRaisesRegex(RuntimeError, "must not be a symlink"):
                    app._scan_docs()


if __name__ == "__main__":
    unittest.main()
