#!/usr/bin/env python3
"""Build the public ContextLattice documentation from repository Markdown."""

from __future__ import annotations

import argparse
import html
import json
import re
import sys
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parents[1]
SOURCE_ROOT = ROOT / "docs/wiki"
OUTPUT_ROOT = ROOT / "docs/public_overview/docs"
CONTRACT_PATH = ROOT / "config/commercial_truth.v1.json"
SOURCE_FILES = (
    "README.md",
    "getting-started.md",
    "concepts.md",
    "cli.md",
    "integrations.md",
    "operations.md",
    "troubleshooting.md",
    "releases.md",
)
INLINE_PATTERN = re.compile(
    r"(`[^`\n]+`|\*\*[^*\n]+\*\*|\[[^\]\n]+\]\([^)]+\))"
)
HEADING_PATTERN = re.compile(r"^(#{1,3})\s+(.+?)\s*$")
UNORDERED_PATTERN = re.compile(r"^[-*]\s+(.+)$")
ORDERED_PATTERN = re.compile(r"^\d+[.)]\s+(.+)$")


@dataclass(frozen=True)
class Doc:
    source: Path
    title: str
    summary: str
    eyebrow: str
    slug: str
    order: int
    body: str

    @property
    def href(self) -> str:
        return "/docs/" if not self.slug else f"/docs/{self.slug}/"

    @property
    def output_path(self) -> Path:
        return OUTPUT_ROOT / ("index.html" if not self.slug else f"{self.slug}/index.html")

    @property
    def source_relative(self) -> str:
        return self.source.relative_to(ROOT).as_posix()


def parse_front_matter(path: Path) -> tuple[dict[str, str], str]:
    text = path.read_text(encoding="utf-8")
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        raise ValueError(f"{path.relative_to(ROOT)}: missing front matter")
    try:
        end = lines.index("---", 1)
    except ValueError as exc:
        raise ValueError(f"{path.relative_to(ROOT)}: unterminated front matter") from exc
    metadata: dict[str, str] = {}
    for line in lines[1:end]:
        if not line.strip():
            continue
        key, separator, value = line.partition(":")
        if not separator or not key.strip() or not value.strip():
            raise ValueError(f"{path.relative_to(ROOT)}: invalid front matter line {line!r}")
        metadata[key.strip()] = value.strip()
    return metadata, "\n".join(lines[end + 1 :]).strip() + "\n"


def load_docs() -> list[Doc]:
    docs: list[Doc] = []
    for filename in SOURCE_FILES:
        source = SOURCE_ROOT / filename
        if not source.is_file():
            raise ValueError(f"missing documentation source: {source.relative_to(ROOT)}")
        metadata, body = parse_front_matter(source)
        missing = [key for key in ("title", "summary", "eyebrow", "order") if not metadata.get(key)]
        if missing:
            raise ValueError(f"{source.relative_to(ROOT)}: missing metadata {missing}")
        slug = metadata.get("slug", source.stem)
        if filename == "README.md":
            slug = ""
        if slug and not re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", slug):
            raise ValueError(f"{source.relative_to(ROOT)}: invalid slug {slug!r}")
        if len(re.findall(r"^#\s+", body, re.MULTILINE)) != 1:
            raise ValueError(f"{source.relative_to(ROOT)}: expected exactly one level-one heading")
        docs.append(
            Doc(
                source=source,
                title=metadata["title"],
                summary=metadata["summary"],
                eyebrow=metadata["eyebrow"],
                slug=slug,
                order=int(metadata["order"]),
                body=body,
            )
        )
    orders = [doc.order for doc in docs]
    slugs = [doc.slug for doc in docs]
    if len(set(orders)) != len(orders) or len(set(slugs)) != len(slugs):
        raise ValueError("documentation order and slugs must be unique")
    return sorted(docs, key=lambda doc: doc.order)


def slugify(value: str) -> str:
    plain = re.sub(r"[`*_]", "", value).lower()
    slug = re.sub(r"[^a-z0-9]+", "-", plain).strip("-")
    return slug or "section"


def map_markdown_target(target: str) -> str:
    parsed = urlsplit(target)
    if parsed.scheme or parsed.netloc or target.startswith(("#", "mailto:", "tel:")):
        return target
    path = parsed.path
    suffix = f"#{parsed.fragment}" if parsed.fragment else ""
    if path in {"README.md", "./README.md"}:
        return f"/docs/{suffix}"
    if path.endswith(".md") and "/" not in path.lstrip("./"):
        return f"/docs/{Path(path).stem}/{suffix}"
    return target


def render_inline(value: str) -> str:
    parts: list[str] = []
    cursor = 0
    for match in INLINE_PATTERN.finditer(value):
        parts.append(html.escape(value[cursor : match.start()]))
        token = match.group(0)
        if token.startswith("`"):
            parts.append(f"<code>{html.escape(token[1:-1])}</code>")
        elif token.startswith("**"):
            parts.append(f"<strong>{html.escape(token[2:-2])}</strong>")
        else:
            link_match = re.fullmatch(r"\[([^\]]+)\]\(([^)]+)\)", token)
            if not link_match:
                parts.append(html.escape(token))
            else:
                label, raw_target = link_match.groups()
                target = map_markdown_target(raw_target.strip())
                parsed = urlsplit(target)
                external = parsed.scheme in {"http", "https"} and bool(parsed.netloc)
                extra = ' target="_blank" rel="noreferrer"' if external else ""
                parts.append(
                    f'<a href="{html.escape(target, quote=True)}"{extra}>{html.escape(label)}</a>'
                )
        cursor = match.end()
    parts.append(html.escape(value[cursor:]))
    return "".join(parts)


def starts_block(line: str) -> bool:
    return bool(
        not line.strip()
        or line.startswith("```")
        or HEADING_PATTERN.match(line)
        or UNORDERED_PATTERN.match(line)
        or ORDERED_PATTERN.match(line)
        or line.startswith("> ")
        or line.strip() == "---"
    )


def render_markdown(doc: Doc) -> tuple[str, list[tuple[int, str, str]]]:
    lines = doc.body.splitlines()
    rendered: list[str] = []
    headings: list[tuple[int, str, str]] = []
    used_ids: dict[str, int] = {}
    index = 0
    while index < len(lines):
        line = lines[index]
        if not line.strip():
            index += 1
            continue
        if line.startswith("```"):
            language = line[3:].strip()
            index += 1
            code_lines: list[str] = []
            while index < len(lines) and not lines[index].startswith("```"):
                code_lines.append(lines[index])
                index += 1
            if index >= len(lines):
                raise ValueError(f"{doc.source_relative}: unclosed code fence")
            index += 1
            language_class = (
                f' class="language-{html.escape(language, quote=True)}"' if language else ""
            )
            rendered.append(
                f'<div class="code-frame"><div class="code-frame-label">'
                f'<span>{html.escape(language or "text")}</span></div>'
                f'<pre><code{language_class}>{html.escape(chr(10).join(code_lines))}</code></pre></div>'
            )
            continue
        heading_match = HEADING_PATTERN.match(line)
        if heading_match:
            level = len(heading_match.group(1))
            label = heading_match.group(2)
            base_id = slugify(label)
            count = used_ids.get(base_id, 0)
            used_ids[base_id] = count + 1
            heading_id = base_id if count == 0 else f"{base_id}-{count + 1}"
            headings.append((level, heading_id, re.sub(r"[`*_]", "", label)))
            rendered.append(
                f'<h{level} id="{heading_id}">{render_inline(label)}'
                f'<a class="heading-anchor" href="#{heading_id}" aria-label="Link to {html.escape(label, quote=True)}">#</a>'
                f"</h{level}>"
            )
            index += 1
            continue
        unordered_match = UNORDERED_PATTERN.match(line)
        if unordered_match:
            items: list[str] = []
            while index < len(lines):
                item = UNORDERED_PATTERN.match(lines[index])
                if not item:
                    break
                items.append(f"<li>{render_inline(item.group(1))}</li>")
                index += 1
            rendered.append(f'<ul class="doc-list">{"".join(items)}</ul>')
            continue
        ordered_match = ORDERED_PATTERN.match(line)
        if ordered_match:
            items = []
            while index < len(lines):
                item = ORDERED_PATTERN.match(lines[index])
                if not item:
                    break
                items.append(f"<li>{render_inline(item.group(1))}</li>")
                index += 1
            rendered.append(f'<ol class="doc-list">{"".join(items)}</ol>')
            continue
        if line.startswith("> "):
            quote_lines: list[str] = []
            while index < len(lines) and lines[index].startswith("> "):
                quote_lines.append(lines[index][2:])
                index += 1
            rendered.append(
                f'<blockquote>{" ".join(render_inline(item) for item in quote_lines)}</blockquote>'
            )
            continue
        if line.strip() == "---":
            rendered.append("<hr>")
            index += 1
            continue
        paragraph = [line.strip()]
        index += 1
        while index < len(lines) and not starts_block(lines[index]):
            paragraph.append(lines[index].strip())
            index += 1
        rendered.append(f"<p>{render_inline(' '.join(paragraph))}</p>")
    return "\n".join(rendered), headings


def doc_navigation(docs: list[Doc], current: Doc) -> str:
    links = []
    for doc in docs:
        current_attr = ' aria-current="page"' if doc == current else ""
        links.append(
            f'<a href="{doc.href}"{current_attr}>'
            f'<span>{doc.order:02d}</span><strong>{html.escape(doc.title)}</strong></a>'
        )
    return "\n".join(links)


def table_of_contents(headings: list[tuple[int, str, str]]) -> str:
    links = [
        f'<a class="toc-level-{level}" href="#{heading_id}">{html.escape(label)}</a>'
        for level, heading_id, label in headings
        if level in {2, 3}
    ]
    if not links:
        return '<p class="toc-empty">Single-screen reference.</p>'
    return "\n".join(links)


def previous_next(docs: list[Doc], current: Doc) -> str:
    position = docs.index(current)
    previous = docs[position - 1] if position > 0 else None
    following = docs[position + 1] if position + 1 < len(docs) else None

    def link(label: str, doc: Doc | None) -> str:
        if doc is None:
            return '<span class="docs-step docs-step-empty" aria-hidden="true"></span>'
        return (
            f'<a class="docs-step" href="{doc.href}"><small>{label}</small>'
            f"<strong>{html.escape(doc.title)}</strong></a>"
        )

    return f"{link('Previous', previous)}{link('Next', following)}"


def render_page(docs: list[Doc], doc: Doc, stable_tag: str) -> str:
    article, headings = render_markdown(doc)
    canonical = f"https://contextlattice.io{doc.href}"
    source_url = (
        "https://github.com/sheawinkler/ContextLattice/blob/main/"
        f"{doc.source_relative}"
    )
    return f"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>{html.escape(doc.title)} | ContextLattice Docs</title>
  <meta name="description" content="{html.escape(doc.summary, quote=True)}" />
  <meta name="doc-source" content="{html.escape(doc.source_relative, quote=True)}" />
  <link rel="canonical" href="{canonical}" />
  <link rel="icon" type="image/png" sizes="512x512" href="/assets/contextlattice-icon-512.png" />
  <link rel="stylesheet" href="/styles-editorial.css?v=20260724c" />
  <link rel="stylesheet" href="/docs/docs.css?v=20260724b" />
  <script src="/docs/docs.js?v=20260723a" defer></script>
</head>
<body class="docs-page" data-doc-href="{doc.href}">
  <!-- Generated by scripts/build_public_docs.py from {doc.source_relative}; do not edit. -->
  <a class="skip-link" href="#main-content">Skip to documentation</a>
  <header class="site-header">
    <a class="site-brand" href="/" aria-label="ContextLattice home">
      <span class="site-brand-mark" aria-hidden="true"></span>
      <span><strong>ContextLattice</strong><small>Durable agent continuity</small></span>
    </a>
    <nav class="site-nav" aria-label="Primary">
      <a href="/docs/" aria-current="page">Docs</a>
      <a href="/premium.html">Pricing</a>
      <a href="/updates.html">Updates</a>
      <a href="https://app.contextlattice.io/console">Dashboard</a>
      <a href="https://github.com/sheawinkler/ContextLattice" target="_blank" rel="noreferrer">GitHub</a>
    </nav>
  </header>

  <div class="docs-tape" aria-label="Documentation status">
    <span><strong>Manual</strong> Repository-backed</span>
    <span><strong>Release</strong> {html.escape(stable_tag)}</span>
    <span><strong>Interface</strong> CLI first</span>
    <span><strong>Source</strong> Public/main</span>
  </div>

  <main class="docs-shell" id="main-content">
    <aside class="docs-sidebar" aria-label="Documentation">
      <div class="docs-sidebar-head">
        <div><span>CL / DOCS</span><strong>Continuity Fieldbook</strong></div>
        <button class="docs-menu-toggle" type="button" aria-expanded="false" aria-controls="docs-sidebar-body">Index</button>
      </div>
      <div class="docs-sidebar-body" id="docs-sidebar-body">
        <div class="docs-search">
          <label for="docs-search">Search <kbd>⌘ K</kbd></label>
          <input id="docs-search" type="search" placeholder="Search the continuity fieldbook" autocomplete="off" />
          <div class="docs-search-results" id="docs-search-results" aria-live="polite"></div>
        </div>
        <nav class="docs-nav" aria-label="Documentation sections">
          {doc_navigation(docs, doc)}
        </nav>
        <a class="docs-source-link" href="{source_url}" target="_blank" rel="noreferrer">View Markdown source ↗</a>
      </div>
    </aside>

    <article class="docs-article" data-source="{html.escape(doc.source_relative, quote=True)}">
      <div class="docs-article-meta">
        <div><span>{html.escape(doc.eyebrow)}</span><strong>{doc.order:02d} / {len(docs):02d}</strong></div>
        <a href="{source_url}" target="_blank" rel="noreferrer">Edit this page ↗</a>
      </div>
      <div class="docs-prose">
        {article}
      </div>
      <nav class="docs-pagination" aria-label="Previous and next documentation pages">
        {previous_next(docs, doc)}
      </nav>
    </article>

    <aside class="docs-toc" aria-label="On this page">
      <span>On this page</span>
      {table_of_contents(headings)}
      <a class="docs-top-link" href="#main-content">Back to top ↑</a>
    </aside>
  </main>

  <footer class="site-footer">
    <span>ContextLattice / docs / {html.escape(stable_tag)}</span>
    <nav aria-label="Footer">
      <a href="/installation.html">Install</a>
      <a href="/troubleshooting.html">Troubleshoot</a>
      <a href="https://github.com/sheawinkler/ContextLattice/issues" target="_blank" rel="noreferrer">Report an issue</a>
    </nav>
  </footer>
</body>
</html>
"""


def searchable_text(body: str) -> str:
    text = re.sub(r"```.*?```", " ", body, flags=re.DOTALL)
    text = re.sub(r"[#>*_`\[\]()]", " ", text)
    return re.sub(r"\s+", " ", text).strip()


def build_outputs() -> dict[Path, str]:
    docs = load_docs()
    contract = json.loads(CONTRACT_PATH.read_text(encoding="utf-8"))
    stable_tag = str(contract["product"]["stable_tag"])
    outputs: dict[Path, str] = {}
    search_documents = []
    for doc in docs:
        outputs[doc.output_path] = render_page(docs, doc, stable_tag)
        headings = [
            label
            for level, _, label in render_markdown(doc)[1]
            if level in {2, 3}
        ]
        search_documents.append(
            {
                "title": doc.title,
                "summary": doc.summary,
                "href": doc.href,
                "headings": headings,
                "text": searchable_text(doc.body),
            }
        )
    search_index = {
        "schemaId": "contextlattice_public_docs_search.v1",
        "release": stable_tag,
        "documents": search_documents,
    }
    outputs[OUTPUT_ROOT / "search-index.json"] = (
        json.dumps(search_index, indent=2, ensure_ascii=False) + "\n"
    )
    return outputs


def relative_output(path: Path) -> str:
    return path.relative_to(ROOT).as_posix()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--write", action="store_true", help="write generated public documentation")
    mode.add_argument("--check", action="store_true", help="fail when generated documentation has drift")
    args = parser.parse_args()

    try:
        outputs = build_outputs()
    except (OSError, ValueError, KeyError, json.JSONDecodeError) as exc:
        print(json.dumps({"ok": False, "error": str(exc)}, indent=2), file=sys.stderr)
        return 1

    drift = []
    for path, expected in outputs.items():
        current = path.read_text(encoding="utf-8") if path.is_file() else None
        if current != expected:
            drift.append(relative_output(path))
            if args.write:
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(expected, encoding="utf-8")

    result = {
        "schemaId": "contextlattice_public_docs_build.v1",
        "mode": "write" if args.write else "check",
        "ok": args.write or not drift,
        "sources": [f"docs/wiki/{name}" for name in SOURCE_FILES],
        "managedOutputs": [relative_output(path) for path in outputs],
        "drift": drift,
    }
    print(json.dumps(result, indent=2))
    return 0 if result["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
