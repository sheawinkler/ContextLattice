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


if __name__ == "__main__":
    unittest.main()
