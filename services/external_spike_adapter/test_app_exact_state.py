from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from fastapi import HTTPException

import app


def _write_registry(path: Path, paths: list[str]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(
            {
                "schema_id": "contextlattice_exact_state_index.v1",
                "paths": paths,
            }
        ),
        encoding="utf-8",
    )


class ExternalAdapterExactStateBoundaryTests(unittest.TestCase):
    def test_scan_excludes_registered_and_internal_state(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "memory-bank"
            registry = root / "_contextlattice" / "exact_state_paths.json"
            _write_registry(registry, ["project::runtime/state.json"])
            exact = root / "project" / "runtime" / "state.json"
            exact.parent.mkdir(parents=True)
            exact.write_text("exact secret", encoding="utf-8")
            ordinary = root / "project" / "notes" / "learning.md"
            ordinary.parent.mkdir(parents=True)
            ordinary.write_text("durable learning", encoding="utf-8")
            history = root / "_contextlattice" / "memory_write_history.ndjson"
            history.write_text("internal history secret", encoding="utf-8")

            with (
                patch.object(app, "DATA_ROOT", root),
                patch.object(app, "EXACT_STATE_INDEX_PATH", registry),
            ):
                docs, fingerprint = app._scan_docs()

            self.assertTrue(fingerprint)
            self.assertEqual(
                [(row["project"], row["file"]) for row in docs],
                [("project", "notes/learning.md")],
            )

    def test_search_reloads_registry_at_final_response_boundary(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            registry = Path(tmp) / "exact_state_paths.json"
            _write_registry(registry, [])

            def backend_search(*_args: object) -> list[app.SearchResult]:
                _write_registry(registry, ["project::runtime/state.json"])
                return [
                    app.SearchResult(
                        project="project",
                        file="runtime//./state.json",
                        summary="must stay private",
                        score=2.0,
                        topic_path="runtime",
                    ),
                    app.SearchResult(
                        project="project",
                        file="learning.md",
                        summary="durable learning",
                        score=1.0,
                        topic_path="general",
                    ),
                ]

            with (
                patch.object(app, "EXACT_STATE_INDEX_PATH", registry),
                patch.object(app, "_trigger_refresh"),
                patch.object(app, "_search_docs", side_effect=backend_search),
            ):
                response = app.search(app.SearchRequest(query="state", limit=10))

            self.assertEqual([row.file for row in response.results], ["learning.md"])
            self.assertEqual(response.meta["exact_state_paths"], 1)

    def test_search_fails_closed_when_registry_is_missing(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            missing = Path(tmp) / "missing.json"
            with (
                patch.object(app, "EXACT_STATE_INDEX_PATH", missing),
                patch.object(app, "_trigger_refresh"),
                patch.object(app, "_search_docs", return_value=[]),
            ):
                with self.assertRaises(HTTPException) as caught:
                    app.search(app.SearchRequest(query="state", limit=10))

            self.assertEqual(caught.exception.status_code, 503)

    def test_symlinked_registry_fails_closed(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / "registry-target.json"
            _write_registry(target, [])
            registry = Path(tmp) / "exact-state.json"
            try:
                registry.symlink_to(target)
            except (NotImplementedError, OSError) as exc:
                self.skipTest(f"symlink unavailable: {exc}")
            with self.assertRaisesRegex(app.ExactStateRegistryError, "must not be a symlink"):
                app.load_exact_state_paths(registry)

    def test_exact_rows_do_not_consume_result_limit(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            registry = Path(tmp) / "exact_state_paths.json"
            _write_registry(registry, ["project::runtime/state.json"])
            docs = [
                {
                    "project": "project",
                    "file": "runtime/state.json",
                    "summary": "exact state state",
                    "text": "exact state state",
                    "topic_path": "runtime",
                },
                {
                    "project": "project",
                    "file": "learning.md",
                    "summary": "durable state",
                    "text": "durable state",
                    "topic_path": "general",
                },
            ]
            with (
                patch.object(app, "EXACT_STATE_INDEX_PATH", registry),
                patch.object(app, "_trigger_refresh"),
                patch.dict(app._state, {"docs_cache": docs}, clear=False),
            ):
                response = app.search(app.SearchRequest(query="state", limit=1))

            self.assertEqual([row.file for row in response.results], ["learning.md"])

    def test_aliases_cannot_bypass_exact_state_membership(self) -> None:
        paths = {"project::runtime/state.json"}
        aliases = [
            "runtime/state.json",
            "runtime//./state.json",
            "runtime\\state.json",
            " runtime / state.json ",
            "/runtime/state.json",
            "runtime::state.json",
        ]
        for alias in aliases:
            with self.subTest(alias=alias):
                self.assertTrue(app.is_exact_state_path(paths, "project", alias))

        self.assertTrue(app.is_exact_state_path(paths, "project::alias", "learning.md"))

    def test_scan_resolves_symlink_targets_without_breaking_content_blob_links(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "memory-bank"
            registry = root / "_contextlattice" / "exact_state_paths.json"
            _write_registry(registry, ["project::runtime/state.json"])
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
            _write_registry(registry, [])
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
