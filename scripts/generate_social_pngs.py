#!/usr/bin/env python3
"""Generate social PNG cards with safe text layout and no overflow."""

from __future__ import annotations

from pathlib import Path
from typing import Iterable, List, Tuple

from PIL import Image, ImageDraw, ImageFont


ROOT = Path(__file__).resolve().parents[1]
ASSETS = ROOT / "docs" / "public_overview" / "assets"
SOCIAL = ASSETS / "social"


def _load_font(size: int, bold: bool = False) -> ImageFont.FreeTypeFont | ImageFont.ImageFont:
    candidates = []
    if bold:
        candidates.extend(
            [
                "/System/Library/Fonts/Supplemental/Arial Bold.ttf",
                "/System/Library/Fonts/Supplemental/Helvetica.ttc",
                "/Library/Fonts/Arial Bold.ttf",
            ]
        )
    candidates.extend(
        [
            "/System/Library/Fonts/Supplemental/Arial.ttf",
            "/System/Library/Fonts/Supplemental/Helvetica.ttc",
            "/Library/Fonts/Arial.ttf",
        ]
    )
    for path in candidates:
        try:
            return ImageFont.truetype(path, size=size)
        except Exception:
            continue
    return ImageFont.load_default()


def _text_width(draw: ImageDraw.ImageDraw, text: str, font: ImageFont.ImageFont) -> float:
    return draw.textlength(text, font=font)


def _wrap_text(
    draw: ImageDraw.ImageDraw,
    text: str,
    font: ImageFont.ImageFont,
    max_width: int,
) -> List[str]:
    words = text.split()
    if not words:
        return []
    lines: List[str] = []
    line = words[0]
    for word in words[1:]:
        cand = f"{line} {word}"
        if _text_width(draw, cand, font) <= max_width:
            line = cand
        else:
            lines.append(line)
            line = word
    lines.append(line)
    return lines


def _fit_wrapped(
    draw: ImageDraw.ImageDraw,
    text: str,
    base_size: int,
    max_width: int,
    min_size: int,
    bold: bool = False,
) -> Tuple[ImageFont.ImageFont, List[str]]:
    size = base_size
    while size >= min_size:
        font = _load_font(size, bold=bold)
        lines = _wrap_text(draw, text, font, max_width)
        if lines and max(_text_width(draw, ln, font) for ln in lines) <= max_width:
            return font, lines
        size -= 2
    font = _load_font(min_size, bold=bold)
    return font, _wrap_text(draw, text, font, max_width)


def _fit_wrapped_limited(
    draw: ImageDraw.ImageDraw,
    text: str,
    base_size: int,
    max_width: int,
    min_size: int,
    max_lines: int,
    bold: bool = False,
) -> Tuple[ImageFont.ImageFont, List[str]]:
    size = base_size
    while size >= min_size:
        font = _load_font(size, bold=bold)
        lines = _wrap_text(draw, text, font, max_width)
        if lines and len(lines) <= max_lines and max(_text_width(draw, ln, font) for ln in lines) <= max_width:
            return font, lines
        size -= 2
    font = _load_font(min_size, bold=bold)
    return font, _wrap_text(draw, text, font, max_width)


def _fit_single_line(
    draw: ImageDraw.ImageDraw,
    text: str,
    base_size: int,
    max_width: int,
    min_size: int,
    bold: bool = False,
) -> ImageFont.ImageFont:
    size = base_size
    while size >= min_size:
        font = _load_font(size, bold=bold)
        if _text_width(draw, text, font) <= max_width:
            return font
        size -= 2
    return _load_font(min_size, bold=bold)


def _draw_bg(draw: ImageDraw.ImageDraw, w: int, h: int) -> None:
    for y in range(h):
        t = y / max(1, h - 1)
        r = int(8 + (17 - 8) * t)
        g = int(11 + (24 - 11) * t)
        b = int(18 + (38 - 18) * t)
        draw.line([(0, y), (w, y)], fill=(r, g, b))
    grid = (28, 40, 60)
    step_x = max(34, int(w * 0.04))
    step_y = max(34, int(h * 0.09))
    for x in range(0, w, step_x):
        draw.line([(x, 0), (x, h)], fill=grid, width=1)
    for y in range(0, h, step_y):
        draw.line([(0, y), (w, y)], fill=grid, width=1)


def _draw_hub(draw: ImageDraw.ImageDraw, w: int, h: int, panel_right: int) -> None:
    lane_left = panel_right + int(w * 0.02)
    lane_right = w - int(w * 0.03)
    lane_width = max(120, lane_right - lane_left)
    cx = lane_left + int(lane_width * 0.62)
    cy = int(h * (0.20 if h > int(w * 1.2) else 0.24))
    r = max(
        42,
        min(
            int(min(w, h) * 0.14),
            int(lane_width * 0.33),
        ),
    )
    cx = min(cx, lane_right - r)
    draw.ellipse(
        (cx - r, cy - r, cx + r, cy + r),
        fill=(188, 204, 218),
        outline=(230, 240, 248),
        width=2,
    )
    start_x = panel_right + int(w * 0.014)
    y0_base = int(h * (0.30 if h > int(w * 1.2) else 0.36))
    for i in range(5):
        y0 = int(y0_base + i * h * 0.024)
        y1 = cy + int((i - 2) * h * 0.022)
        draw.line([(start_x, y0), (cx - r, y1)], fill=(192, 218, 242), width=2)


def _draw_chip(
    draw: ImageDraw.ImageDraw,
    text: str,
    x: int,
    y: int,
    max_width: int,
    base_size: int,
    min_size: int,
) -> int:
    hpad = max(12, int(max_width * 0.035))
    vpad = max(8, int(base_size * 0.35))
    text_max = max_width - (2 * hpad)
    font, lines = _fit_wrapped(
        draw,
        text,
        base_size=base_size,
        max_width=text_max,
        min_size=min_size,
        bold=True,
    )
    if not lines:
        lines = [text]
    line_h = int(font.size * 1.18)
    chip_h = max(line_h + (2 * vpad), (line_h * len(lines)) + (2 * vpad))
    draw.rounded_rectangle(
        (x, y, x + max_width, y + chip_h),
        radius=int(chip_h * 0.26),
        fill=(32, 43, 64),
        outline=(161, 186, 215),
        width=1,
    )
    ty = y + vpad
    for line in lines:
        draw.text((x + hpad, ty), line, fill=(239, 244, 252), font=font)
        ty += line_h
    return chip_h


def _measure_chip(
    draw: ImageDraw.ImageDraw,
    text: str,
    max_width: int,
    base_size: int,
    min_size: int,
) -> Tuple[int, ImageFont.ImageFont, List[str]]:
    hpad = max(12, int(max_width * 0.035))
    vpad = max(8, int(base_size * 0.35))
    text_max = max_width - (2 * hpad)
    font, lines = _fit_wrapped(
        draw,
        text,
        base_size=base_size,
        max_width=text_max,
        min_size=min_size,
        bold=True,
    )
    if not lines:
        lines = [text]
    line_h = int(font.size * 1.18)
    chip_h = max(line_h + (2 * vpad), (line_h * len(lines)) + (2 * vpad))
    return chip_h, font, lines


def _draw_general_card(w: int, h: int, out: Path) -> None:
    img = Image.new("RGB", (w, h), "#0b1118")
    draw = ImageDraw.Draw(img)
    _draw_bg(draw, w, h)

    if h > int(w * 1.25):
        panel_right_ratio = 0.68
        panel_bottom_ratio = 0.87
    elif h > int(w * 0.95):
        panel_right_ratio = 0.66
        panel_bottom_ratio = 0.88
    else:
        panel_right_ratio = 0.70
        panel_bottom_ratio = 0.90

    panel = (
        int(w * 0.05),
        int(h * 0.12),
        int(w * panel_right_ratio),
        int(h * panel_bottom_ratio),
    )
    panel_w = panel[2] - panel[0]
    pad = int(panel_w * 0.04)
    draw.rounded_rectangle(panel, radius=max(16, int(w * 0.018)), fill=(8, 14, 28, 242), outline=(146, 168, 198), width=2)

    title_font = _fit_single_line(
        draw,
        "CONTEXT LATTICE",
        base_size=max(42, int(w * 0.095)),
        max_width=panel_w - (2 * pad),
        min_size=max(24, int(w * 0.040)),
        bold=True,
    )
    if h <= int(w * 0.75):
        subtitle_max_lines = 2
    elif h <= int(w * 1.05):
        subtitle_max_lines = 3
    else:
        subtitle_max_lines = 4
    subtitle_font, subtitle_lines = _fit_wrapped_limited(
        draw,
        "Private-by-default memory & context layer for agents.",
        max(24, int(w * 0.048)),
        panel_w - 2 * pad,
        max(14, int(w * 0.022)),
        subtitle_max_lines,
        bold=True,
    )
    chip_base = max(14, int(w * 0.022))
    chip_min = max(11, int(w * 0.015))
    footer_font = _fit_single_line(
        draw,
        "contextlattice.io",
        base_size=max(14, int(w * 0.02)),
        max_width=panel_w - (2 * pad),
        min_size=max(12, int(w * 0.016)),
        bold=True,
    )

    x = panel[0] + pad
    y = panel[1] + int(h * 0.045)
    draw.text((x, y), "CONTEXT LATTICE", fill=(245, 249, 252), font=title_font)
    y += int(title_font.size * 1.35)

    for line in subtitle_lines:
        draw.text((x, y), line, fill=(217, 229, 245), font=subtitle_font)
        y += int(subtitle_font.size * 1.2)

    y += int(h * 0.04)
    chips = [
        "Go ingress + Rust retrieval hot path",
        "HTTP/messaging app interfacing (claw-ready)",
        "Multi-backend fusion enhances recall quality",
    ]
    chip_w = panel_w - (2 * pad)
    chip_gap = int(h * 0.022)
    footer_y = panel[3] - int(footer_font.size * 1.4)
    min_bottom_gap = max(10, int(h * 0.02))

    while chip_base > chip_min:
        measured = [_measure_chip(draw, chip, chip_w, chip_base, chip_min)[0] for chip in chips]
        needed = sum(measured) + (chip_gap * (len(chips) - 1))
        if y + needed <= footer_y - min_bottom_gap:
            break
        chip_base -= 1
        chip_gap = max(6, chip_gap - 1)

    for chip in chips:
        chip_h = _draw_chip(draw, chip, x, y, chip_w, chip_base, chip_min)
        y += chip_h + chip_gap

    if y > footer_y - min_bottom_gap:
        footer_y = y + min_bottom_gap
    footer_y = min(footer_y, panel[3] - footer_font.size)
    draw.text((x, footer_y), "contextlattice.io", fill=(177, 194, 216), font=footer_font)
    _draw_hub(draw, w, h, panel[2])
    out.parent.mkdir(parents=True, exist_ok=True)
    img.save(out, format="PNG", optimize=True)


def _draw_metrics_card(w: int, h: int, out: Path) -> None:
    img = Image.new("RGB", (w, h), "#0b1118")
    draw = ImageDraw.Draw(img)
    _draw_bg(draw, w, h)

    panel = (int(w * 0.05), int(h * 0.10), int(w * 0.95), int(h * 0.90))
    draw.rounded_rectangle(panel, radius=max(16, int(w * 0.018)), fill=(8, 14, 28, 242), outline=(146, 168, 198), width=2)
    pad = int((panel[2] - panel[0]) * 0.04)
    x = panel[0] + pad
    y = panel[1] + int(h * 0.06)

    title = _load_font(max(34, int(h * 0.085)), bold=True)
    sub = _load_font(max(18, int(h * 0.038)), bold=True)
    body = _load_font(max(16, int(h * 0.032)), bold=False)
    mono = _load_font(max(18, int(h * 0.036)), bold=True)

    draw.text((x, y), "CONTEXTLATTICE v3.18.0", fill=(244, 249, 252), font=title)
    y += int(title.size * 1.3)
    draw.text((x, y), "Published retrieval performance", fill=(207, 223, 244), font=sub)
    y += int(sub.size * 1.4)

    blocks = [
        ("Earlier runtime cutover (Go/Rust vs legacy Python)",
         "A mean: 3557ms vs 17565ms (4.94x)",
         "B p50: 2334ms vs 20006ms (8.57x)",
         "C p95: 8494ms vs 20008ms (2.36x)"),
        ("Current A/B/C (:8075 Go vs :18075 Python)",
         "A mean: 0.157s vs 0.255s (38.547%)",
         "B p50: 0.139s vs 0.202s (31.135%)",
         "C p95: 0.268s vs 0.429s (37.456%)"),
    ]
    for header, a, b, c in blocks:
        draw.text((x, y), header, fill=(236, 244, 251), font=sub)
        y += int(sub.size * 1.15)
        for line in (a, b, c):
            draw.text((x + int(w * 0.012), y), line, fill=(201, 217, 236), font=mono)
            y += int(mono.size * 1.2)
        y += int(h * 0.022)

    draw.text((x, panel[3] - int(h * 0.06)), "Release: github.com/sheawinkler/ContextLattice/releases/tag/v3.18.0", fill=(172, 190, 214), font=body)
    out.parent.mkdir(parents=True, exist_ok=True)
    img.save(out, format="PNG", optimize=True)


def main() -> None:
    presets: Iterable[Tuple[int, int, str]] = [
        (1200, 630, "contextlattice-og-1200x630.png"),
        (1200, 675, "social/contextlattice-x-1200x675.png"),
        (1200, 627, "social/contextlattice-linkedin-1200x627.png"),
        (1080, 1080, "social/contextlattice-square-1080x1080.png"),
        (1080, 1920, "social/contextlattice-story-1080x1920.png"),
    ]
    for w, h, rel in presets:
        _draw_general_card(w, h, ASSETS / rel)

    metric_presets: Iterable[Tuple[int, int, str]] = [
        (1200, 630, "social/contextlattice-metrics-1200x630.png"),
        (1200, 675, "social/contextlattice-metrics-1200x675.png"),
    ]
    for w, h, rel in metric_presets:
        _draw_metrics_card(w, h, ASSETS / rel)


if __name__ == "__main__":
    main()
