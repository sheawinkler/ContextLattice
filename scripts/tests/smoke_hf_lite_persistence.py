"""Check that the built hf-lite image retains a memory across container replacement."""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
from urllib.request import ProxyHandler, Request, build_opener

DOCUMENT = "decisions/db.md"
MEMORY = {
    "projectName": "smoke-persistence",
    "fileName": DOCUMENT,
    "topicPath": "decisions",
    "content": "Decision: use SQLite with WAL for the billing service because write volume is under 50 rps.",
}
QUERY = {"project": MEMORY["projectName"], "query": "why did we choose SQLite for billing"}
LOCAL_HTTP = build_opener(ProxyHandler({}))


def docker(*args: str) -> str:
    return subprocess.check_output(["docker", *args], text=True, timeout=30).strip()


def request(url: str, path: str, payload: dict[str, str] | None = None) -> bytes:
    body = json.dumps(payload).encode() if payload is not None else None
    headers = {"Content-Type": "application/json"} if body is not None else {}
    with LOCAL_HTTP.open(Request(url + path, data=body, headers=headers), timeout=5) as response:
        return response.read()


def wait_until_ready(url: str) -> None:
    deadline = time.monotonic() + 45
    while True:
        try:
            request(url, "/readyz")
            return
        except OSError:
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.25)


def search_container(image: str, volume: str, *, write_memory: bool) -> list[str]:
    container = docker(
        "run", "--detach", "--pull=never", "--publish", "127.0.0.1::7860",
        "--mount", f"type=volume,source={volume},target=/data", image,
    )
    try:
        url = "http://" + docker("port", container, "7860/tcp")
        wait_until_ready(url)
        if write_memory:
            request(url, "/memory/write", MEMORY)
        result = json.loads(request(url, "/memory/search", QUERY))
        return [item["file"] for item in result["results"]]
    except Exception:
        print(docker("logs", "--tail", "35", container), file=sys.stderr)
        raise
    finally:
        docker("rm", "-f", container)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("image", help="locally built image from Dockerfile.hf-lite")
    image = parser.parse_args().image
    volume = docker("volume", "create")
    try:
        before = search_container(image, volume, write_memory=True)
        if DOCUMENT not in before:
            raise SystemExit(f"memory was not visible in the original container: {before}")
        after = search_container(image, volume, write_memory=False)
        if DOCUMENT not in after:
            raise SystemExit(f"memory vanished when the container was replaced: {after}")
        print(f"memory {DOCUMENT} remained searchable after container replacement")
    finally:
        docker("volume", "rm", volume)


if __name__ == "__main__":
    main()
