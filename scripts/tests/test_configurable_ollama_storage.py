from __future__ import annotations

import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


class ConfigurableOllamaStorageTests(unittest.TestCase):
    def test_compose_preserves_home_default_and_accepts_override(self) -> None:
        compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
        self.assertIn('${OLLAMA_DATA:-${HOME}/.ollama}:/root/.ollama', compose)

        env_example = (ROOT / ".env.example").read_text(encoding="utf-8")
        self.assertIn("\nOLLAMA_DATA=\n", env_example)

    def test_startup_verifier_tracks_ollama_mount_and_repair_suffix(self) -> None:
        verifier = (ROOT / "scripts" / "verify_storage_mounts.sh").read_text(encoding="utf-8")
        self.assertIn('"ollama|OLLAMA_DATA|/root/.ollama"', verifier)
        self.assertIn('OLLAMA_DATA) echo "${HOME}/.ollama"', verifier)
        self.assertIn('OLLAMA_DATA) echo "ollama"', verifier)


if __name__ == "__main__":
    unittest.main()
