#!/usr/bin/env python3
"""Generate launch tracker and channel copybook from a modular launch config."""

from __future__ import annotations

import argparse
import json
from datetime import date
from pathlib import Path
from typing import Any


def _load_json(path: Path) -> dict[str, Any]:
    with path.open("r", encoding="utf-8") as f:
        return json.load(f)


def _fmt(text: str, context: dict[str, str]) -> str:
    return text.format_map(context)


def _render_tracker(config: dict[str, Any]) -> str:
    product_name = config["product_name"]
    category = config["category"]
    one_liner = config["one_liner"]
    primary_cta = config["primary_cta"]
    metadata = config["metadata"]
    channels = config["channels"]
    launch_window = config["launch_window"]
    risks = config["risks"]
    kpis = config["kpis"]
    sources = config["sources"]

    today = date.today().isoformat()
    lines: list[str] = []
    lines.append(f"# {product_name} Publish Execution Tracker (MCP Service)")
    lines.append("")
    lines.append(f"Last updated: {today}")
    lines.append(f"Launch window: {launch_window}")
    lines.append("")
    lines.append("## 1) Positioning Guardrails")
    lines.append("")
    lines.append("- Classify as `MCP server` / `MCP service` / `local-first memory orchestration service`.")
    lines.append("- Do not use plugin/template-style labeling in public listings.")
    lines.append(f"- Category anchor: `{category}`.")
    lines.append("")
    lines.append("## 2) Canonical Launch Metadata")
    lines.append("")
    lines.append("| Field | Value |")
    lines.append("| --- | --- |")
    lines.append(f"| Product name | {product_name} |")
    lines.append(f"| Category | {category} |")
    lines.append(f"| Primary URL | `{metadata['primary_url']}` |")
    lines.append(f"| Docs URL | `{metadata['docs_url']}` |")
    lines.append(f"| Troubleshooting URL | `{metadata['troubleshooting_url']}` |")
    lines.append(f"| Repo URL | `{metadata['repo_url']}` |")
    lines.append(f"| Public overview URL | `{metadata['public_overview_url']}` |")
    lines.append(f"| One-liner | {one_liner} |")
    lines.append(f"| Primary CTA | {primary_cta} |")
    lines.append("")
    lines.append("## 3) Channel Tracker")
    lines.append("")
    lines.append("| Tier | Channel | Listing URL | Submission path | Lead time | Cost signal | Owner | Scheduled publish (MT / PT) | Status |")
    lines.append("| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for channel in channels:
        lines.append(
            "| {tier} | {name} | {url} | {path} | {lead} | {cost} | {owner} | {mt} / {pt} | {status} |".format(
                tier=channel["tier"],
                name=channel["name"],
                url=channel["listing_url"],
                path=channel["submission_path"],
                lead=channel["lead_time"],
                cost=channel["cost_signal"],
                owner=channel["owner"],
                mt=channel["schedule_mt"],
                pt=channel["schedule_pt"],
                status=channel["status"],
            )
        )
    lines.append("")
    lines.append("## 4) Launch-Day Run of Show")
    lines.append("")
    for idx, step in enumerate(config["run_of_show"], start=1):
        lines.append(f"{idx}. `{step['time_mt']} MT / {step['time_pt']} PT` - {step['action']}")
    lines.append("")
    lines.append("## 5) KPI Targets")
    lines.append("")
    lines.append("| KPI | 24h target | 7d target | Source |")
    lines.append("| --- | --- | --- | --- |")
    for row in kpis:
        lines.append(f"| {row['kpi']} | {row['target_24h']} | {row['target_7d']} | {row['source']} |")
    lines.append("")
    lines.append("## 6) Risk Register")
    lines.append("")
    lines.append("| Risk | Impact | Mitigation |")
    lines.append("| --- | --- | --- |")
    for row in risks:
        lines.append(f"| {row['risk']} | {row['impact']} | {row['mitigation']} |")
    lines.append("")
    lines.append("## 7) Source Links")
    lines.append("")
    for src in sources:
        lines.append(f"- {src['label']}: `{src['url']}`")
    lines.append("")
    return "\n".join(lines)


def _render_copybook(config: dict[str, Any]) -> str:
    product_name = config["product_name"]
    metadata = config["metadata"]
    channels = config["channels"]
    copy_blocks = config["copy_blocks"]

    context = {
        "product_name": product_name,
        "primary_url": metadata["primary_url"],
        "docs_url": metadata["docs_url"],
        "troubleshooting_url": metadata["troubleshooting_url"],
        "repo_url": metadata["repo_url"],
        "one_liner": config["one_liner"],
        "primary_cta": config["primary_cta"],
    }

    today = date.today().isoformat()
    lines: list[str] = []
    lines.append(f"# {product_name} Launch Channel Copybook")
    lines.append("")
    lines.append(f"Last updated: {today}")
    lines.append(f"Launch target: {config['launch_window']}")
    lines.append("")
    lines.append("Use these blocks as final copy for synchronized launch submissions.")
    lines.append("")
    lines.append("## Canonical One-liner")
    lines.append("")
    lines.append(f"- {_fmt(config['one_liner'], context)}")
    lines.append("")

    for channel in channels:
        key = channel.get("copy_key", "").strip()
        if not key:
            continue
        if key not in copy_blocks:
            continue
        block = copy_blocks[key]
        lines.append(f"## {channel['name']}")
        lines.append("")
        lines.append(f"- Scheduled publish: `{channel['schedule_mt']} MT / {channel['schedule_pt']} PT`")
        lines.append(f"- Listing URL: `{channel['listing_url']}`")
        lines.append("")
        if block.get("title"):
            lines.append(f"Title: {_fmt(block['title'], context)}")
            lines.append("")
        if block.get("tagline"):
            lines.append(f"Tagline: {_fmt(block['tagline'], context)}")
            lines.append("")
        if block.get("body"):
            body = _fmt(block["body"], context).replace("\\n", "\n").rstrip()
            lines.append("```text")
            lines.append(body)
            lines.append("```")
            lines.append("")
    return "\n".join(lines)


def main() -> None:
    parser = argparse.ArgumentParser(description="Generate launch docs from launch_service config")
    parser.add_argument(
        "--config",
        default="launch_service/config/contextlattice.launch.json",
        help="Path to launch config JSON",
    )
    parser.add_argument(
        "--tracker-out",
        default="docs/publish_execution_tracker.md",
        help="Output markdown for publish execution tracker",
    )
    parser.add_argument(
        "--copybook-out",
        default="docs/launch_channel_copybook.md",
        help="Output markdown for channel copybook",
    )
    args = parser.parse_args()

    config_path = Path(args.config)
    tracker_out = Path(args.tracker_out)
    copybook_out = Path(args.copybook_out)

    config = _load_json(config_path)

    tracker_text = _render_tracker(config)
    copybook_text = _render_copybook(config)

    tracker_out.parent.mkdir(parents=True, exist_ok=True)
    copybook_out.parent.mkdir(parents=True, exist_ok=True)
    tracker_out.write_text(tracker_text + "\n", encoding="utf-8")
    copybook_out.write_text(copybook_text + "\n", encoding="utf-8")

    print(f"Wrote {tracker_out}")
    print(f"Wrote {copybook_out}")


if __name__ == "__main__":
    main()
