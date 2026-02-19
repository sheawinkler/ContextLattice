#!/usr/bin/env python3
"""Audit and optionally update Docker service image tags.

This script is intentionally dependency-free so it can run on clean hosts.
It inspects compose files, checks latest stable upstream versions for
selected registries, and can patch pinned semver tags in-place.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, asdict
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Tuple


DOCKERHUB_API = "https://hub.docker.com/v2/repositories/{repo}/tags?page_size=100&page={page}&ordering=last_updated"
GITHUB_RELEASES_API = "https://api.github.com/repos/{repo}/releases?per_page=100&page={page}"


# Only semver-like stable tags are considered for auto-updates.
# This excludes rc/beta/dev channels and flavor tags (e.g., -alpine, -rocm).
STABLE_SEMVER_RE = re.compile(r"^v?\d+\.\d+\.\d+(?:\.\d+)?$")
VERSION_PART_RE = re.compile(r"^v?(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:\.(\d+))?(?:[-+].*)?$")

# Special-case sources that do not have a practical unauthenticated tags API.
SOURCE_OVERRIDES = {
    "ghcr.io/tbxark/mcp-proxy": ("github_releases", "TBXark/mcp-proxy"),
}

# We still report unsupported registries, but skip version fetch.
UNSUPPORTED_REGISTRIES = ("cgr.dev/", "ghcr.io/promptfoo/")


@dataclass
class ImageEntry:
    compose_file: str
    line_no: int
    service: str
    image_ref: str
    image_name: str
    image_tag: str
    tag_mode: str
    source_kind: str
    source_ref: Optional[str]
    current_version: Optional[Tuple[int, ...]]
    latest_stable_tag: Optional[str] = None
    latest_stable_version: Optional[Tuple[int, ...]] = None
    update_available: bool = False
    target_tag: Optional[str] = None
    note: str = ""


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Audit and update compose image tags.")
    parser.add_argument(
        "--compose-file",
        action="append",
        default=[],
        help="Compose file to scan (repeatable). Defaults: docker-compose.yml + docker-compose.lite.yml",
    )
    parser.add_argument(
        "--report-file",
        default="tmp/service-version-report.json",
        help="Path to write JSON report.",
    )
    parser.add_argument(
        "--apply",
        action="store_true",
        help="Apply safe tag bumps for pinned semver images.",
    )
    parser.add_argument(
        "--allow-major",
        action="store_true",
        help="Allow major version jumps when applying updates.",
    )
    parser.add_argument(
        "--timeout-secs",
        type=float,
        default=20.0,
        help="HTTP timeout for registry queries.",
    )
    return parser.parse_args()


def is_registry_host(component: str) -> bool:
    return "." in component or ":" in component or component == "localhost"


def split_image_ref(image_ref: str) -> Tuple[str, str]:
    ref = image_ref.strip()
    if "@" in ref:
        ref = ref.split("@", 1)[0]
    if ":" in ref and "/" in ref and ref.rsplit(":", 1)[1].find("/") == -1:
        name, tag = ref.rsplit(":", 1)
        return name, tag
    if ":" in ref and "/" not in ref:
        name, tag = ref.rsplit(":", 1)
        return name, tag
    return ref, "latest"


def classify_tag_mode(tag: str) -> str:
    lower = tag.lower()
    if lower == "latest":
        return "floating-latest"
    if STABLE_SEMVER_RE.fullmatch(tag):
        return "pinned-semver"
    return "floating-channel"


def parse_version_tuple(tag: str) -> Optional[Tuple[int, ...]]:
    match = VERSION_PART_RE.fullmatch(tag)
    if not match:
        return None
    parts: List[int] = []
    for group in match.groups():
        if group is None:
            continue
        parts.append(int(group))
    return tuple(parts) if parts else None


def normalize_image_name(image_name: str) -> str:
    if image_name.startswith("docker.io/"):
        return image_name
    first = image_name.split("/", 1)[0]
    if is_registry_host(first):
        return image_name
    if "/" in image_name:
        return f"docker.io/{image_name}"
    return f"docker.io/library/{image_name}"


def resolve_source(image_name: str) -> Tuple[str, Optional[str]]:
    normalized = normalize_image_name(image_name)
    for prefix, source in SOURCE_OVERRIDES.items():
        if normalized == prefix:
            return source
    for prefix in UNSUPPORTED_REGISTRIES:
        if normalized.startswith(prefix):
            return "unsupported", None
    if normalized.startswith("docker.io/"):
        repo = normalized.removeprefix("docker.io/")
        return "dockerhub", repo
    return "unsupported", None


def request_json(url: str, timeout_secs: float) -> Any:
    req = urllib.request.Request(
        url,
        headers={
            "Accept": "application/json",
            "User-Agent": "memmcp-service-version-audit/1.0",
        },
    )
    with urllib.request.urlopen(req, timeout=timeout_secs) as response:
        payload = response.read().decode("utf-8")
    return json.loads(payload)


def fetch_latest_from_dockerhub(repo: str, timeout_secs: float) -> Tuple[Optional[str], Optional[Tuple[int, ...]], str]:
    page = 1
    best_tag: Optional[str] = None
    best_version: Optional[Tuple[int, ...]] = None
    while page <= 6:
        url = DOCKERHUB_API.format(repo=urllib.parse.quote(repo, safe="/"), page=page)
        data = request_json(url, timeout_secs)
        results = data.get("results", [])
        for row in results:
            tag = str(row.get("name", "")).strip()
            if not STABLE_SEMVER_RE.fullmatch(tag):
                continue
            version = parse_version_tuple(tag)
            if not version:
                continue
            if best_version is None or version > best_version:
                best_tag = tag
                best_version = version
        if not data.get("next"):
            break
        page += 1
    if best_tag:
        return best_tag, best_version, ""
    return None, None, "No stable semver tag found via Docker Hub API."


def fetch_latest_from_github_releases(repo: str, timeout_secs: float) -> Tuple[Optional[str], Optional[Tuple[int, ...]], str]:
    page = 1
    best_tag: Optional[str] = None
    best_version: Optional[Tuple[int, ...]] = None
    while page <= 4:
        url = GITHUB_RELEASES_API.format(repo=repo, page=page)
        releases = request_json(url, timeout_secs)
        if not isinstance(releases, list):
            break
        for row in releases:
            if row.get("draft") or row.get("prerelease"):
                continue
            tag = str(row.get("tag_name", "")).strip()
            if not STABLE_SEMVER_RE.fullmatch(tag):
                continue
            version = parse_version_tuple(tag)
            if not version:
                continue
            if best_version is None or version > best_version:
                best_tag = tag
                best_version = version
        if len(releases) < 100:
            break
        page += 1
    if best_tag:
        return best_tag, best_version, ""
    return None, None, "No stable release tag found via GitHub Releases API."


def fetch_latest_stable(source_kind: str, source_ref: Optional[str], timeout_secs: float) -> Tuple[Optional[str], Optional[Tuple[int, ...]], str]:
    if source_kind == "dockerhub" and source_ref:
        return fetch_latest_from_dockerhub(source_ref, timeout_secs)
    if source_kind == "github_releases" and source_ref:
        return fetch_latest_from_github_releases(source_ref, timeout_secs)
    return None, None, "Source not supported for automatic stable-version lookup."


def load_compose_images(compose_path: Path) -> List[ImageEntry]:
    lines = compose_path.read_text(encoding="utf-8").splitlines()
    entries: List[ImageEntry] = []
    in_services = False
    current_service: Optional[str] = None
    for idx, line in enumerate(lines, start=1):
        if re.match(r"^\s*services:\s*$", line):
            in_services = True
            current_service = None
            continue
        if not in_services:
            continue
        service_match = re.match(r"^  ([A-Za-z0-9_.-]+):\s*$", line)
        if service_match:
            current_service = service_match.group(1)
            continue
        image_match = re.match(r'^\s*image:\s*["\']?([^"\']+)["\']?\s*$', line)
        if not image_match or not current_service:
            continue
        image_ref = image_match.group(1).strip()
        image_name, image_tag = split_image_ref(image_ref)
        tag_mode = classify_tag_mode(image_tag)
        source_kind, source_ref = resolve_source(image_name)
        current_version = parse_version_tuple(image_tag)
        entries.append(
            ImageEntry(
                compose_file=str(compose_path),
                line_no=idx,
                service=current_service,
                image_ref=image_ref,
                image_name=image_name,
                image_tag=image_tag,
                tag_mode=tag_mode,
                source_kind=source_kind,
                source_ref=source_ref,
                current_version=current_version,
            )
        )
    return entries


def compare_versions(
    entry: ImageEntry,
    allow_major: bool,
) -> Tuple[bool, Optional[str], str]:
    if entry.tag_mode != "pinned-semver":
        return False, None, "Floating tag; updates are picked up via docker pull."
    if not entry.current_version or not entry.latest_stable_version or not entry.latest_stable_tag:
        return False, None, "Pinned semver tag but unable to compare versions."
    current_major = entry.current_version[0]
    latest_major = entry.latest_stable_version[0]
    if not allow_major and latest_major != current_major:
        return False, None, "Major bump available but blocked (use --allow-major)."
    if entry.latest_stable_version <= entry.current_version:
        return False, None, "Already at latest stable for current policy."
    return True, entry.latest_stable_tag, ""


def apply_updates(entries: Iterable[ImageEntry]) -> Tuple[int, List[str]]:
    by_file: Dict[str, List[ImageEntry]] = {}
    for entry in entries:
        if not entry.update_available or not entry.target_tag:
            continue
        by_file.setdefault(entry.compose_file, []).append(entry)

    changed_files: List[str] = []
    changes = 0
    for file_path, file_entries in by_file.items():
        path = Path(file_path)
        lines = path.read_text(encoding="utf-8").splitlines(keepends=True)
        line_updates = {entry.line_no: entry for entry in file_entries}
        file_changed = False
        for idx, line in enumerate(lines, start=1):
            entry = line_updates.get(idx)
            if not entry or not entry.target_tag:
                continue
            image_match = re.match(r'^(\s*image:\s*["\']?)([^"\']+)(["\']?\s*)$', line.rstrip("\n"))
            if not image_match:
                continue
            old_ref = image_match.group(2).strip()
            if old_ref != entry.image_ref:
                continue
            new_ref = f"{entry.image_name}:{entry.target_tag}"
            updated = f"{image_match.group(1)}{new_ref}{image_match.group(3)}\n"
            lines[idx - 1] = updated
            changes += 1
            file_changed = True
        if file_changed:
            path.write_text("".join(lines), encoding="utf-8")
            changed_files.append(file_path)
    return changes, changed_files


def summarize(entries: List[ImageEntry]) -> str:
    lines = []
    header = f"{'SERVICE':20} {'IMAGE':38} {'CURRENT':16} {'LATEST_STABLE':16} {'ACTION':10}"
    lines.append(header)
    lines.append("-" * len(header))
    for entry in sorted(entries, key=lambda x: (x.compose_file, x.service)):
        action = "update" if entry.update_available else "none"
        lines.append(
            f"{entry.service[:20]:20} "
            f"{entry.image_name[:38]:38} "
            f"{entry.image_tag[:16]:16} "
            f"{(entry.latest_stable_tag or '-'):16} "
            f"{action:10}"
        )
    return "\n".join(lines)


def main() -> int:
    args = parse_args()
    compose_files = args.compose_file or ["docker-compose.yml", "docker-compose.lite.yml"]
    compose_paths = [Path(path) for path in compose_files]

    entries: List[ImageEntry] = []
    for path in compose_paths:
        if not path.exists():
            print(f"[warn] compose file not found: {path}", file=sys.stderr)
            continue
        entries.extend(load_compose_images(path))

    latest_cache: Dict[Tuple[str, Optional[str]], Tuple[Optional[str], Optional[Tuple[int, ...]], str]] = {}
    for entry in entries:
        cache_key = (entry.source_kind, entry.source_ref)
        if cache_key not in latest_cache:
            try:
                latest_cache[cache_key] = fetch_latest_stable(entry.source_kind, entry.source_ref, args.timeout_secs)
            except urllib.error.HTTPError as exc:
                latest_cache[cache_key] = (None, None, f"HTTP {exc.code}: {exc.reason}")
            except Exception as exc:  # pragma: no cover - network edge cases
                latest_cache[cache_key] = (None, None, str(exc))
        latest_tag, latest_version, note = latest_cache[cache_key]
        entry.latest_stable_tag = latest_tag
        entry.latest_stable_version = latest_version
        compare_ok, target_tag, compare_note = compare_versions(entry, args.allow_major)
        entry.update_available = compare_ok
        entry.target_tag = target_tag
        entry.note = compare_note or note

    if args.apply:
        changes, changed_files = apply_updates(entries)
    else:
        changes, changed_files = 0, []

    report = {
        "generatedAt": datetime.now(timezone.utc).isoformat(),
        "composeFiles": [str(path) for path in compose_paths if path.exists()],
        "applied": bool(args.apply),
        "allowMajor": bool(args.allow_major),
        "changesApplied": changes,
        "changedFiles": changed_files,
        "entries": [asdict(entry) for entry in entries],
    }

    report_path = Path(args.report_file)
    report_path.parent.mkdir(parents=True, exist_ok=True)
    report_path.write_text(json.dumps(report, indent=2), encoding="utf-8")

    print(summarize(entries))
    print(f"\nReport: {report_path}")
    if args.apply:
        print(f"Applied changes: {changes}")
        if changed_files:
            for file_path in changed_files:
                print(f"  - {file_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
