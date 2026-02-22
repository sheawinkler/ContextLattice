#!/usr/bin/env python3
"""Preflight checks for launch-directory submission readiness."""

from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable


ROOT = Path(__file__).resolve().parents[1]


@dataclass
class CheckResult:
    ok: bool
    name: str
    details: str


def _exists(paths: Iterable[str]) -> list[CheckResult]:
    out: list[CheckResult] = []
    for rel in paths:
        p = ROOT / rel
        out.append(CheckResult(p.exists(), f"path:{rel}", "present" if p.exists() else "missing"))
    return out


def _contains(path: str, required_tokens: Iterable[str]) -> list[CheckResult]:
    p = ROOT / path
    if not p.exists():
        return [CheckResult(False, f"content:{path}", "file missing")]
    text = p.read_text(encoding="utf-8")
    out: list[CheckResult] = []
    for token in required_tokens:
        out.append(
            CheckResult(
                token in text,
                f"content:{path}:{token}",
                "found" if token in text else "not found",
            )
        )
    return out


def _check_launch_config() -> list[CheckResult]:
    path = ROOT / "launch_service/config/contextlattice.launch.json"
    if not path.exists():
        return [CheckResult(False, "launch_config", "launch config missing")]
    cfg = json.loads(path.read_text(encoding="utf-8"))
    channels = cfg.get("channels", [])
    names = {c.get("name") for c in channels}
    expected = {
        "MCP Registry (official)",
        "Glama MCP",
        "PulseMCP",
        "MCP.so",
        "Product Hunt",
        "FutureTools",
        "Futurepedia",
        "Toolify",
    }
    out: list[CheckResult] = []
    for name in sorted(expected):
        out.append(CheckResult(name in names, f"channel:{name}", "configured" if name in names else "missing"))
    for channel in channels:
        cname = channel.get("name", "unknown")
        for field in ("listing_url", "submission_path", "owner", "status"):
            ok = bool(channel.get(field))
            out.append(
                CheckResult(
                    ok,
                    f"channel:{cname}:{field}",
                    "set" if ok else "empty",
                )
            )
    return out


def _check_glama_claim() -> list[CheckResult]:
    path = ROOT / "docs/public_overview/.well-known/glama.json"
    if not path.exists():
        return [CheckResult(False, "glama_claim", "missing .well-known/glama.json")]
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        return [CheckResult(False, "glama_claim", f"invalid json: {exc}")]
    email = str(payload.get("email", "")).strip()
    return [
        CheckResult(bool(email), "glama_claim:email", "set" if email else "missing"),
        CheckResult(
            email.endswith("@gmail.com") or "@" in email,
            "glama_claim:email_format",
            email or "missing",
        ),
    ]


def _check_urls(urls: Iterable[str], timeout: int) -> list[CheckResult]:
    out: list[CheckResult] = []
    for url in urls:
        req = urllib.request.Request(
            url,
            method="HEAD",
            headers={"User-Agent": "ContextLattice-Submission-Preflight/1.0"},
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                code = getattr(resp, "status", 200)
                out.append(CheckResult(200 <= code < 400, f"url:{url}", f"status={code}"))
        except urllib.error.HTTPError as exc:
            if exc.code == 405:
                # Some hosts disallow HEAD; retry with GET for reachability.
                get_req = urllib.request.Request(
                    url,
                    method="GET",
                    headers={"User-Agent": "ContextLattice-Submission-Preflight/1.0"},
                )
                try:
                    with urllib.request.urlopen(get_req, timeout=timeout) as resp:
                        code = getattr(resp, "status", 200)
                        out.append(CheckResult(200 <= code < 400, f"url:{url}", f"status={code}"))
                except Exception as inner_exc:  # noqa: BLE001
                    out.append(CheckResult(False, f"url:{url}", f"error={inner_exc}"))
            else:
                out.append(CheckResult(False, f"url:{url}", f"http_error={exc.code}"))
        except Exception as exc:  # noqa: BLE001
            out.append(CheckResult(False, f"url:{url}", f"error={exc}"))
    return out


def main() -> int:
    parser = argparse.ArgumentParser(description="Check submission readiness for launch directories.")
    parser.add_argument(
        "--online",
        action="store_true",
        help="Also verify public URLs are reachable.",
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=12,
        help="HTTP timeout seconds for --online checks.",
    )
    args = parser.parse_args()

    checks: list[CheckResult] = []
    checks.extend(
        _exists(
            [
                "README.md",
                "LICENSE",
                "SECURITY.md",
                "docs/legal/PRIVACY_POLICY.md",
                "docs/legal/TERMS_OF_SERVICE.md",
                "docs/publish_execution_tracker.md",
                "docs/launch_channel_copybook.md",
                "docs/public_overview/CNAME",
                "docs/public_overview/index.html",
                "docs/public_overview/installation.html",
                "docs/public_overview/integration.html",
                "docs/public_overview/troubleshooting.html",
                "docs/public_overview/contact.html",
                "docs/public_overview/.nojekyll",
                "docs/public_overview/assets/contextlattice-og-1200x630.png",
                "docs/public_overview/assets/contextlattice-icon-512.png",
                "docs/public_overview/.well-known/glama.json",
                "registry/contextlattice.server.template.json",
                "docs/submission_requirements.md",
            ]
        )
    )
    checks.extend(
        _contains(
            "README.md",
            [
                "gmake quickstart",
                "Private/Public Sync Notes",
                "Local-first",
            ],
        )
    )
    checks.extend(
        _contains(
            "docs/public_overview/index.html",
            [
                'property="og:title"',
                'name="twitter:card"',
                "application/ld+json",
                "assets/contextlattice-og-1200x630.png",
            ],
        )
    )
    checks.extend(_contains("docs/public_overview/contact.html", ["sheawinkler@gmail.com"]))
    checks.extend(_contains("docs/public_overview/CNAME", ["contextlattice.io"]))
    checks.extend(_check_launch_config())
    checks.extend(_check_glama_claim())

    if args.online:
        checks.extend(
            _check_urls(
                [
                    "https://contextlattice.io/",
                    "https://contextlattice.io/installation.html",
                    "https://contextlattice.io/integration.html",
                    "https://contextlattice.io/troubleshooting.html",
                    "https://contextlattice.io/.well-known/glama.json",
                    "https://github.com/sheawinkler/ContextLattice",
                ],
                timeout=args.timeout,
            )
        )

    failing = [c for c in checks if not c.ok]
    for c in checks:
        state = "PASS" if c.ok else "FAIL"
        print(f"[{state}] {c.name} :: {c.details}")

    print(
        f"\nSummary: {len(checks) - len(failing)}/{len(checks)} checks passed."
        + (" Online checks enabled." if args.online else "")
    )

    if failing:
        print("\nFailing checks:")
        for c in failing:
            print(f"- {c.name}: {c.details}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
