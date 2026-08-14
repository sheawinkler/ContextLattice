"""Streaming public-release scan for secrets and private machine material."""

from __future__ import annotations

import hashlib
import json
import re
from pathlib import Path
from typing import BinaryIO, Iterable


PUBLIC_MATERIAL_SCAN_SCHEMA_ID = "contextlattice_public_release_material_scan.v1"
DEFAULT_CHUNK_SIZE = 64 * 1024
SCAN_OVERLAP_BYTES = 1024
MAX_FINDINGS = 32
_DIGEST_DOMAIN = b"contextlattice-public-release-material-scan-v1\0"
_SAFE_ASSET_LABEL = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+-]{0,191}\Z")

_PATTERNS: tuple[tuple[str, re.Pattern[bytes]], ...] = (
    (
        "token.stripe_live",
        re.compile(rb"(?<![A-Za-z0-9_-])sk_live_[A-Za-z0-9_-]{16,192}(?![A-Za-z0-9_-])"),
    ),
    (
        "token.github",
        re.compile(
            rb"(?<![A-Za-z0-9_])(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})"
        ),
    ),
    (
        "token.aws_access_key",
        re.compile(rb"(?<![A-Z0-9])(?:AKIA|ASIA)[A-Z0-9]{16}(?![A-Z0-9])"),
    ),
    (
        "token.slack",
        re.compile(rb"(?<![A-Za-z0-9])xox[baprs]-[A-Za-z0-9-]{10,200}(?![A-Za-z0-9-])"),
    ),
    (
        "credential.authorization_header",
        re.compile(
            rb"(?:authorization|proxy-authorization)[ \t]*:[ \t]*(?:bearer|basic)[ \t]+"
            rb"[A-Za-z0-9._~+/=-]{12,256}",
            re.IGNORECASE,
        ),
    ),
    (
        "credential.named_value",
        re.compile(
            rb"(?<![A-Za-z0-9_-])"
            rb"(?:api[_-]?key|client[_-]?secret|access[_-]?token|refresh[_-]?token|password|passwd)"
            rb"[\"']?[ \t]*[:=][ \t]*[\"']?[A-Za-z0-9._~+/=-]{24,256}"
            rb"(?![A-Za-z0-9._~+/=-])",
            re.IGNORECASE,
        ),
    ),
    (
        "key.private_pem_header",
        re.compile(
            rb"-----BEGIN[ \t]+"
            rb"(?:(?:RSA|EC|DSA|OPENSSH|ENCRYPTED|PGP)[ \t]+)?"
            rb"PRIVATE[ \t]+KEY(?:[ \t]+BLOCK)?-----",
            re.IGNORECASE,
        ),
    ),
    (
        "path.private_posix",
        re.compile(
            rb"(?:file:)?/{1,3}(?:Users|Volumes|private|home)/[^\x00\r\n\"'<>]{1,256}",
            re.IGNORECASE,
        ),
    ),
    (
        "path.private_windows_drive",
        re.compile(
            rb"(?<![A-Za-z0-9])[A-Za-z]:(?:\\{1,2}|/)+"
            rb"(?:Users|Documents[ ]and[ ]Settings|home)(?:\\{1,2}|/)+"
            rb"[^\x00\r\n\"'<>]{1,256}",
            re.IGNORECASE,
        ),
    ),
    (
        "path.private_unc",
        re.compile(
            rb"\\{2,4}[A-Za-z0-9._-]+\\{1,2}[A-Za-z0-9$._-]+\\{1,2}"
            rb"[^\x00\r\n\"'<>]{1,256}",
            re.IGNORECASE,
        ),
    ),
)


class PublicReleaseMaterialError(RuntimeError):
    """A public release contained secret-shaped or machine-private bytes."""

    def __init__(self, report: dict[str, object]) -> None:
        self.report = report
        super().__init__(json.dumps(report, sort_keys=True, separators=(",", ":")))


def _asset_label(path: Path) -> str:
    name = path.name
    if _SAFE_ASSET_LABEL.fullmatch(name):
        return name
    digest = hashlib.sha256(name.encode("utf-8", errors="surrogatepass")).hexdigest()
    return f"asset-name-sha256:{digest}"


def _finding(code: str, asset: str, evidence: bytes) -> dict[str, str]:
    digest = hashlib.sha256(
        _DIGEST_DOMAIN + code.encode("ascii") + b"\0" + asset.encode("ascii") + b"\0" + evidence
    ).hexdigest()
    return {
        "asset": asset,
        "code": code,
        "evidence_digest": f"sha256:{digest}",
    }


def _scan_stream(
    source: BinaryIO,
    *,
    asset: str,
    chunk_size: int,
    copy_to: BinaryIO | None = None,
    stop_at_max_findings: bool = True,
) -> tuple[list[dict[str, str]], int]:
    findings: list[dict[str, str]] = []
    seen_offsets: set[tuple[str, int]] = set()
    tail = b""
    total_read = 0
    while True:
        chunk = source.read(chunk_size)
        if not chunk:
            break
        if copy_to is not None:
            copy_to.write(chunk)
        window = tail + chunk
        window_start = total_read - len(tail)
        if len(findings) < MAX_FINDINGS:
            for code, pattern in _PATTERNS:
                for match in pattern.finditer(window):
                    absolute_start = window_start + match.start()
                    marker = (code, absolute_start)
                    if marker in seen_offsets:
                        continue
                    seen_offsets.add(marker)
                    findings.append(_finding(code, asset, match.group(0)))
                    if len(findings) >= MAX_FINDINGS:
                        if stop_at_max_findings:
                            return findings, total_read + len(chunk)
                        break
                if len(findings) >= MAX_FINDINGS:
                    break
        total_read += len(chunk)
        tail = window[-SCAN_OVERLAP_BYTES:]
    return findings, total_read


def _scan_file(path: Path, *, chunk_size: int) -> tuple[list[dict[str, str]], int]:
    with path.open("rb") as handle:
        return _scan_stream(
            handle,
            asset=_asset_label(path),
            chunk_size=chunk_size,
        )


def scan_public_release_files(
    paths: Iterable[Path],
    *,
    chunk_size: int = DEFAULT_CHUNK_SIZE,
) -> dict[str, object]:
    """Scan final public bytes without retaining or printing matched material."""

    if isinstance(chunk_size, bool) or not isinstance(chunk_size, int) or chunk_size <= 0:
        raise ValueError("public material scan chunk size must be a positive integer")

    ordered_paths = sorted((Path(path) for path in paths), key=lambda path: path.name)
    findings: list[dict[str, str]] = []
    seen_findings: set[tuple[str, str, str]] = set()
    bytes_scanned = 0
    files_scanned = 0
    truncated = False
    for path in ordered_paths:
        asset = _asset_label(path)
        if path.is_symlink() or not path.is_file():
            candidate_findings = [_finding("scan.non_regular_file", asset, b"non-regular")]
            scanned = 0
        else:
            try:
                candidate_findings, scanned = _scan_file(path, chunk_size=chunk_size)
            except OSError as exc:
                candidate_findings = [
                    _finding("scan.io_error", asset, type(exc).__name__.encode("ascii", errors="replace"))
                ]
                scanned = 0
        files_scanned += 1
        bytes_scanned += scanned
        for finding in candidate_findings:
            marker = (finding["asset"], finding["code"], finding["evidence_digest"])
            if marker in seen_findings:
                continue
            seen_findings.add(marker)
            findings.append(finding)
            if len(findings) >= MAX_FINDINGS:
                truncated = True
                break
        if truncated:
            break

    report: dict[str, object] = {
        "schema_id": PUBLIC_MATERIAL_SCAN_SCHEMA_ID,
        "ok": not findings,
        "file_count": files_scanned,
        "byte_count": bytes_scanned,
        "finding_count": len(findings),
        "truncated": truncated,
    }
    if findings:
        report["findings"] = findings
        raise PublicReleaseMaterialError(report)
    return report
