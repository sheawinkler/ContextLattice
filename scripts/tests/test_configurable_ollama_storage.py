from __future__ import annotations

import subprocess
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

    def test_shipped_env_profiles_are_shell_sourceable(self) -> None:
        candidates = (
            ROOT / ".env.example",
            ROOT / "config" / "env" / "premium_prod.env",
            ROOT / "contextlattice-dashboard" / ".env.example",
        )
        for profile in candidates:
            if not profile.exists():
                continue
            with self.subTest(profile=profile.relative_to(ROOT)):
                completed = subprocess.run(
                    [
                        "bash",
                        "-e",
                        "-c",
                        'set -a; source "$1"; set +a',
                        "sourceability-test",
                        str(profile),
                    ],
                    capture_output=True,
                    text=True,
                )
                self.assertEqual(0, completed.returncode, completed.stderr)


if __name__ == "__main__":
    unittest.main()
