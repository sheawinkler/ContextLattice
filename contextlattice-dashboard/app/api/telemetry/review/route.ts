import { NextRequest, NextResponse } from "next/server";
import { callOrchestrator } from "@/lib/orchestrator";

function toText(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function payloadFromSearch(req: NextRequest): Record<string, unknown> {
  const payload: Record<string, unknown> = {};
  for (const [key, value] of req.nextUrl.searchParams.entries()) {
    payload[key] = value;
  }
  return payload;
}

export async function GET(req: NextRequest) {
  const payload = payloadFromSearch(req);
  try {
    const data = await callOrchestrator("/memory/review", {
      method: "POST",
      body: JSON.stringify(payload),
    });
    return NextResponse.json(data);
  } catch (error) {
    return NextResponse.json(reviewUnavailable(payload, error), { status: 200 });
  }
}

export async function POST(req: NextRequest) {
  let payload: Record<string, unknown> = {};
  try {
    const parsed = await req.json();
    payload = parsed && typeof parsed === "object" ? (parsed as Record<string, unknown>) : {};
  } catch {
    payload = {};
  }
  if (!toText(payload.query)) {
    payload.query = "review repeated ContextLattice memory patterns and mitigation steps";
  }
  try {
    const data = await callOrchestrator("/memory/review", {
      method: "POST",
      body: JSON.stringify(payload),
    });
    return NextResponse.json(data);
  } catch (error) {
    return NextResponse.json(reviewUnavailable(payload, error), { status: 200 });
  }
}

function reviewUnavailable(payload: Record<string, unknown>, error: unknown) {
  return {
    ok: false,
    mode: "review",
    project: toText(payload.project),
    topic_path: toText(payload.topic_path ?? payload.topicPath ?? payload.path),
    summary: {
      posture: "unavailable",
      recent_writes: 0,
      pattern_count: 0,
      pressure_score: 0,
    },
    patterns: [],
    agent_guidance: [
      "Review Mode is unavailable from the current gateway; rebuild or restart gateway-go with /memory/review support.",
    ],
    warnings: [error instanceof Error ? error.message.slice(0, 500) : String(error).slice(0, 500)],
  };
}
