from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[2]
DOCKERFILES = (
    "Dockerfile.gate-refresh",
    "Dockerfile.fastembed",
    "Dockerfile.memory-bank-spike-rs",
)


def builder_instructions(filename):
    text = (ROOT / filename).read_text()
    stage = re.split(r"(?m)^FROM[ \t]+", text)[1]
    return [line.strip() for line in stage.replace("\\\n", " ").splitlines()]


class RustServiceDockerfileTests(unittest.TestCase):
    def test_builder_defaults_to_two_cargo_jobs(self):
        for filename in DOCKERFILES:
            with self.subTest(filename=filename):
                instructions = builder_instructions(filename)
                self.assertIn("ARG CARGO_BUILD_JOBS=2", instructions)
                build_index = next(i for i, line in enumerate(instructions) if line.startswith("RUN ") and "cargo build" in line)
                self.assertLess(instructions.index("ARG CARGO_BUILD_JOBS=2"), build_index)

    def test_locked_release_build_consumes_job_argument(self):
        for filename in DOCKERFILES:
            with self.subTest(filename=filename):
                runs = [line for line in builder_instructions(filename) if line.startswith("RUN ")]
                builds = [line for line in runs if "cargo build" in line]
                self.assertEqual(len(builds), 1)
                self.assertIn('cargo build --release --locked -j "${CARGO_BUILD_JOBS}"', builds[0])

    def test_build_logs_compiler_and_cargo_versions(self):
        for filename in DOCKERFILES:
            with self.subTest(filename=filename):
                runs = [" ".join(line.split()) for line in builder_instructions(filename) if line.startswith("RUN ")]
                self.assertTrue(any(line.startswith("RUN rustc -vV && cargo -V && cargo build ") for line in runs))


if __name__ == "__main__":
    unittest.main()
