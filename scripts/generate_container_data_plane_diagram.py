#!/usr/bin/env python3
"""
Generate the public overview container/data-plane diagram from an Excalidraw source.

This keeps a maintainable Excalidraw file in-repo and renders a deterministic SVG
for the landing page.
"""

from __future__ import annotations

import json
import subprocess
import time
from dataclasses import dataclass
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
ASSETS = ROOT / "docs" / "public_overview" / "assets"
EXCALIDRAW_OUT = ASSETS / "container-data-plane.excalidraw"
SVG_OUT = ASSETS / "container-data-plane.svg"


COLOR_STROKE = "#c8d5e5"
COLOR_TEXT = "#eaf2fd"
COLOR_SUBTEXT = "#cdd9e8"
COLOR_FILL = "#1d2734"
COLOR_CORE = "#2a3950"
COLOR_LINE = "#deebfc"
COLOR_ASYNC = "#94d8e8"
COLOR_FEEDBACK = "#dcc18a"


@dataclass
class Box:
    x: float
    y: float
    w: float
    h: float

    @property
    def cx(self) -> float:
        return self.x + self.w / 2

    @property
    def cy(self) -> float:
        return self.y + self.h / 2


class Diagram:
    def __init__(self) -> None:
        self.elements: list[dict] = []
        self._id = 1
        self._seed = 100
        self._nonce = 1000
        self._updated = int(time.time() * 1000)

    def _next_id(self, prefix: str) -> str:
        i = self._id
        self._id += 1
        return f"{prefix}-{i}"

    def _next_seed(self) -> int:
        self._seed += 1
        return self._seed

    def _next_nonce(self) -> int:
        self._nonce += 17
        return self._nonce

    def _base(self, kind: str, x: float, y: float, w: float, h: float) -> dict:
        return {
            "id": self._next_id(kind),
            "type": kind,
            "x": x,
            "y": y,
            "width": w,
            "height": h,
            "angle": 0,
            "strokeColor": COLOR_STROKE,
            "backgroundColor": COLOR_FILL,
            "fillStyle": "solid",
            "strokeWidth": 2,
            "strokeStyle": "solid",
            "roughness": 0,
            "opacity": 100,
            "groupIds": [],
            "frameId": None,
            "roundness": {"type": 3},
            "seed": self._next_seed(),
            "version": 1,
            "versionNonce": self._next_nonce(),
            "isDeleted": False,
            "boundElements": [],
            "updated": self._updated,
            "link": None,
            "locked": False,
        }

    def rect(
        self,
        box: Box,
        *,
        fill: str = COLOR_FILL,
        stroke: str = COLOR_STROKE,
        style: str = "solid",
        radius: bool = True,
        stroke_width: int = 2,
    ) -> None:
        el = self._base("rectangle", box.x, box.y, box.w, box.h)
        el["backgroundColor"] = fill
        el["strokeColor"] = stroke
        el["strokeStyle"] = style
        el["strokeWidth"] = stroke_width
        if not radius:
            el["roundness"] = None
        self.elements.append(el)

    def text(
        self,
        x: float,
        y: float,
        text: str,
        *,
        width: float = 220,
        height: float = 80,
        size: int = 18,
        color: str = COLOR_TEXT,
        align: str = "center",
        valign: str = "middle",
    ) -> None:
        el = self._base("text", x, y, width, height)
        el["strokeColor"] = color
        el["backgroundColor"] = "transparent"
        el["fillStyle"] = "solid"
        el["strokeWidth"] = 1
        el["roundness"] = None
        el["text"] = text
        el["originalText"] = text
        el["fontSize"] = size
        el["fontFamily"] = 2
        el["textAlign"] = align
        el["verticalAlign"] = valign
        el["lineHeight"] = 1.25
        el["baseline"] = size
        el["containerId"] = None
        self.elements.append(el)

    def arrow(
        self,
        points: list[tuple[float, float]],
        *,
        color: str = COLOR_LINE,
        style: str = "solid",
        stroke_width: int = 2,
    ) -> None:
        if len(points) < 2:
            return
        x0, y0 = points[0]
        rel = [[px - x0, py - y0] for px, py in points]
        xs = [p[0] for p in rel]
        ys = [p[1] for p in rel]
        w = max(xs) - min(xs)
        h = max(ys) - min(ys)

        el = self._base("arrow", x0, y0, w, h)
        el["backgroundColor"] = "transparent"
        el["strokeColor"] = color
        el["strokeStyle"] = style
        el["strokeWidth"] = stroke_width
        el["points"] = rel
        el["startBinding"] = None
        el["endBinding"] = None
        el["startArrowhead"] = None
        el["endArrowhead"] = "arrow"
        el["lastCommittedPoint"] = rel[-1]
        el["roundness"] = None
        self.elements.append(el)

    def doc(self) -> dict:
        return {
            "type": "excalidraw",
            "version": 2,
            "source": "https://contextlattice.io",
            "elements": self.elements,
            "appState": {
                "gridSize": None,
                "viewBackgroundColor": "transparent",
                "currentItemFontFamily": 2,
                "currentItemStrokeColor": COLOR_STROKE,
                "currentItemBackgroundColor": COLOR_FILL,
                "currentItemStrokeStyle": "solid",
                "currentItemRoughness": 0,
                "currentItemRoundness": "round",
            },
            "files": {},
        }


def build() -> dict:
    d = Diagram()

    boundary = Box(320, 180, 760, 760)
    clients = Box(70, 390, 190, 180)
    gate = Box(560, 260, 280, 90)
    orchestrator = Box(560, 430, 280, 110)
    outbox = Box(930, 320, 230, 96)
    retrieval = Box(930, 550, 230, 96)
    memory_plane = Box(360, 700, 640, 220)
    memory_bank = Box(380, 760, 130, 112)
    qdrant = Box(530, 760, 130, 112)
    mongo = Box(680, 760, 130, 112)
    mindsdb = Box(830, 760, 150, 112)
    providers = Box(1210, 230, 250, 176)
    messaging = Box(1210, 560, 250, 160)
    legend = Box(980, 80, 300, 126)

    # Frames and primary boxes.
    d.rect(boundary, fill="transparent", style="dashed")
    d.rect(clients)
    d.rect(gate)
    d.rect(orchestrator, fill=COLOR_CORE, stroke="#dbe6f4")
    d.rect(outbox)
    d.rect(retrieval)
    d.rect(memory_plane)
    d.rect(memory_bank)
    d.rect(qdrant)
    d.rect(mongo)
    d.rect(mindsdb)
    d.rect(providers)
    d.rect(messaging)
    d.rect(legend, fill="#141d28")

    # Labels.
    d.text(500, 122, "CONTAINER DIAGRAM: CONTEXT LATTICE RUNTIME", width=360, height=28, size=14, color=COLOR_SUBTEXT)
    d.text(boundary.x + 180, boundary.y + 10, "Context Lattice System Boundary", width=400, height=32, size=20)
    d.text(clients.x, clients.y + 8, "Clients\nagents\nmessaging\napps", width=clients.w, height=clients.h, size=20)
    d.text(gate.x, gate.y + 2, "Ops + Security Gate", width=gate.w, height=gate.h, size=20)
    d.text(orchestrator.x, orchestrator.y + 4, "Orchestrator API\nHTTP ingress + auth + policy", width=orchestrator.w, height=orchestrator.h, size=19)
    d.text(outbox.x, outbox.y + 2, "Outbox Queue\ndurable async fanout", width=outbox.w, height=outbox.h, size=19)
    d.text(retrieval.x, retrieval.y + 2, "Retrieval Engine\nparallel recall + rerank", width=retrieval.w, height=retrieval.h, size=19)
    d.text(memory_plane.x + 180, memory_plane.y + 8, "Memory Plane", width=280, height=34, size=21)
    d.text(memory_bank.x, memory_bank.y + 2, "Memory Bank\ncanonical", width=memory_bank.w, height=memory_bank.h, size=18)
    d.text(qdrant.x, qdrant.y + 2, "Qdrant\nvectors", width=qdrant.w, height=qdrant.h, size=18)
    d.text(mongo.x, mongo.y + 2, "Mongo\nraw ledger", width=mongo.w, height=mongo.h, size=18)
    d.text(mindsdb.x, mindsdb.y + 2, "MindsDB + Letta\nSQL + archival", width=mindsdb.w, height=mindsdb.h, size=18)
    d.text(providers.x, providers.y + 6, "Providers\nQwen/Ollama, LM Studio\nOpenAI-compatible APIs", width=providers.w, height=providers.h, size=19)
    d.text(messaging.x, messaging.y + 6, "Messaging Surfaces\nTelegram / Slack\nOpenClaw family", width=messaging.w, height=messaging.h, size=19)

    d.text(legend.x + 20, legend.y + 10, "Legend", width=90, height=26, size=16, color=COLOR_SUBTEXT, align="left")
    d.text(legend.x + 110, legend.y + 46, "sync request/read", width=160, height=24, size=14, color=COLOR_SUBTEXT, align="left")
    d.text(legend.x + 110, legend.y + 72, "async outbox fanout", width=170, height=24, size=14, color=COLOR_SUBTEXT, align="left")
    d.text(legend.x + 110, legend.y + 98, "learning feedback loop", width=180, height=24, size=14, color=COLOR_SUBTEXT, align="left")

    # Legend lines.
    d.arrow([(legend.x + 20, legend.y + 58), (legend.x + 94, legend.y + 58)], color=COLOR_LINE)
    d.arrow([(legend.x + 20, legend.y + 84), (legend.x + 94, legend.y + 84)], color=COLOR_ASYNC, style="dashed")
    d.arrow([(legend.x + 20, legend.y + 110), (legend.x + 94, legend.y + 110)], color=COLOR_FEEDBACK, style="dotted")

    # Ingress + response.
    d.arrow([(260, 485), (560, 485)], color=COLOR_LINE)
    d.arrow([(560, 520), (260, 520)], color=COLOR_LINE)

    # Control routing.
    d.arrow([(700, 430), (700, 350)], color=COLOR_LINE)
    d.arrow([(840, 305), (930, 365)], color=COLOR_LINE)
    d.arrow([(840, 325), (930, 590)], color=COLOR_LINE)
    d.arrow([(840, 450), (980, 410), (1130, 330), (1210, 320)], color=COLOR_LINE)
    d.arrow([(840, 500), (980, 540), (1110, 610), (1210, 620)], color=COLOR_LINE)

    # Write fanout (async).
    d.text(404, 666, "Async Fanout", width=180, height=28, size=18, color=COLOR_SUBTEXT, align="left", valign="middle")
    d.arrow([(1045, 416), (1045, 690)], color=COLOR_ASYNC, style="dashed")
    d.arrow([(1045, 690), (390, 690)], color=COLOR_ASYNC, style="dashed")
    d.arrow([(390, 690), (390, 760)], color=COLOR_ASYNC, style="dashed")
    d.arrow([(545, 690), (545, 760)], color=COLOR_ASYNC, style="dashed")
    d.arrow([(695, 690), (695, 760)], color=COLOR_ASYNC, style="dashed")
    d.arrow([(905, 690), (905, 760)], color=COLOR_ASYNC, style="dashed")

    # Retrieval synthesis (parallel).
    d.text(670, 642, "Parallel Retrieval", width=180, height=28, size=18, color=COLOR_SUBTEXT, align="left", valign="middle")
    d.arrow([(1045, 646), (1045, 670)], color=COLOR_LINE)
    d.arrow([(1045, 670), (390, 670)], color=COLOR_LINE)
    d.arrow([(390, 670), (390, 760)], color=COLOR_LINE)
    d.arrow([(545, 670), (545, 760)], color=COLOR_LINE)
    d.arrow([(695, 670), (695, 760)], color=COLOR_LINE)
    d.arrow([(905, 670), (905, 760)], color=COLOR_LINE)
    d.arrow([(695, 760), (740, 700), (860, 640), (930, 590)], color=COLOR_LINE)

    # Learning loop.
    d.text(868, 640, "learning write + feedback loop", width=300, height=28, size=16, color=COLOR_FEEDBACK, align="left", valign="middle")
    d.arrow([(1045, 600), (1045, 720), (905, 760)], color=COLOR_FEEDBACK, style="dotted")
    d.arrow([(905, 812), (1165, 812), (1165, 590), (1160, 590)], color=COLOR_FEEDBACK, style="dotted")

    return d.doc()


def main() -> None:
    ASSETS.mkdir(parents=True, exist_ok=True)
    doc = build()
    EXCALIDRAW_OUT.write_text(json.dumps(doc, indent=2))

    cmd = [
        "npx",
        "-y",
        "@moona3k/excalidraw-export@0.2.1",
        str(EXCALIDRAW_OUT),
        "--svg",
        "-o",
        str(SVG_OUT),
        "--no-background",
    ]
    subprocess.run(cmd, check=True)
    print(f"Wrote {EXCALIDRAW_OUT}")
    print(f"Wrote {SVG_OUT}")


if __name__ == "__main__":
    main()
