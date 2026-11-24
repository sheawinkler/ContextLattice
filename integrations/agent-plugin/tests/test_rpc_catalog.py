import io
import json
from pathlib import Path
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from verify_rpc_catalog import MAX_FRAME, NAME, verify


def check(host, *frames):
    return verify(io.BytesIO(b"".join(json.dumps(row).encode() + b"\n" for row in frames)), host, "probe")


def pi(names, **overrides):
    return {"type": "response", "command": "get_commands", "id": "probe", "success": True,
            "data": {"commands": [{"name": name} for name in names]}, **overrides}


def omp(names):
    return {"type": "available_commands_update", "commands": [{"name": name} for name in names]}


class CatalogTests(unittest.TestCase):
    def test_pi_correlates_the_successful_reply(self):
        self.assertTrue(check("pi", omp([]), pi([], id="other"), pi([NAME]))["verified"])
        for frames in ((omp([NAME]),), (pi([NAME], id="wrong"),), (pi([NAME], success=False),),
                       (pi([NAME], command="other"),), (pi([NAME]), pi([NAME])), (pi([NAME, NAME]),)):
            with self.subTest(frames=frames), self.assertRaises(ValueError):
                check("pi", *frames)

    def test_omp_uses_latest_native_event_not_pi_responses(self):
        self.assertTrue(check("omp", omp([]), omp([NAME]), pi([], success=False))["verified"])
        for frames in ((pi([NAME]),), (omp([NAME]), omp([])), (omp([NAME, NAME]),),
                       ({"type": "available_commands_update", "commands": [NAME]},)):
            with self.subTest(frames=frames), self.assertRaises(ValueError):
                check("omp", *frames)

    def test_rejects_oversize_and_nonobject_input(self):
        with self.assertRaises(ValueError):
            verify(io.BytesIO(b"x" * (MAX_FRAME + 1)), "omp", "probe")
        with self.assertRaises(ValueError):
            check("pi", ["not a frame"])
