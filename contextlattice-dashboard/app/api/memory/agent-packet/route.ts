import { NextResponse } from "next/server";
import { fetchOrchestrator } from "@/lib/orchestrator";

const MAX_BODY_CHARS = 16 * 1024;

function boundedText(value: unknown, max: number): string {
  return typeof value === "string" ? value.trim().slice(0, max) : "";
}

function boundedInt(value: unknown, fallback: number, min: number, max: number): number {
  const parsed = Number.parseInt(String(value ?? ""), 10);
  return Number.isFinite(parsed) ? Math.max(min, Math.min(max, parsed)) : fallback;
}

export async function POST(request: Request) {
  const raw = await request.text();
  if (raw.length > MAX_BODY_CHARS) {
    return NextResponse.json({ ok: false, error: "request_too_large" }, { status: 413 });
  }

  let incoming: Record<string, unknown>;
  try {
    const parsed: unknown = raw ? JSON.parse(raw) : {};
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
      return NextResponse.json({ ok: false, error: "json_object_required" }, { status: 400 });
    }
    incoming = parsed as Record<string, unknown>;
  } catch {
    return NextResponse.json({ ok: false, error: "invalid_json" }, { status: 400 });
  }

  const query = boundedText(incoming.query, 1600);
  if (!query) {
    return NextResponse.json({ ok: false, error: "query_required" }, { status: 400 });
  }
  const target = boundedInt(incoming.target_context_pack_tokens, 2000, 512, 4000);
  const hard = boundedInt(incoming.hard_limit_tokens, 4000, target, 4000);
  const payload = {
    query,
    project: boundedText(incoming.project, 120) || "contextlattice",
    topic_path: boundedText(incoming.topic_path, 240),
    retrieval_mode: ["fast", "balanced", "deep"].includes(boundedText(incoming.retrieval_mode, 16))
      ? boundedText(incoming.retrieval_mode, 16)
      : "balanced",
    output_mode: "agent_packet.v1",
    target_context_pack_tokens: target,
    hard_limit_tokens: hard,
  };

  try {
    const response = await fetchOrchestrator("/memory/synthesis-pack/v2", {
      method: "POST",
      body: JSON.stringify(payload),
      signal: AbortSignal.timeout(60_000),
    });
    if (!response.ok) {
      const detail = (await response.text()).slice(0, 280);
      return NextResponse.json(
        { ok: false, error: "agent_packet_unavailable", detail },
        { status: response.status >= 400 && response.status < 500 ? response.status : 502 },
      );
    }
    const packet = await response.json();
    if (packet?.schema_id !== "agent_packet.v1") {
      return NextResponse.json({ ok: false, error: "invalid_agent_packet_contract" }, { status: 502 });
    }
    return NextResponse.json(packet);
  } catch (error) {
    return NextResponse.json(
      { ok: false, error: "agent_packet_unavailable", detail: error instanceof Error ? error.message : String(error) },
      { status: 502 },
    );
  }
}
