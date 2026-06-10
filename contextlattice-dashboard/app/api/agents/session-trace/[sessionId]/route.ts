import { NextResponse } from "next/server";
import { fetchOrchestrator } from "@/lib/orchestrator";

function toText(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

export async function GET(
  _request: Request,
  context: { params: Promise<{ sessionId: string }> },
) {
  const { sessionId } = await context.params;
  const id = toText(sessionId);
  if (!id) {
    return NextResponse.json(
      { ok: false, error: "sessionId is required" },
      { status: 422 },
    );
  }

  try {
    const response = await fetchOrchestrator(
      `/v1/agents/sessions/${encodeURIComponent(id)}/trace`,
      { method: "GET" },
    );
    if (!response.ok) {
      const detail = (await response.text()).slice(0, 480);
      return NextResponse.json(
        {
          ok: false,
          sessionId: id,
          error: `/v1/agents/sessions/{session_id}/trace -> ${response.status}${detail ? ` ${detail}` : ""}`,
        },
        { status: 200 },
      );
    }
    return NextResponse.json(await response.json());
  } catch (error) {
    return NextResponse.json(
      {
        ok: false,
        sessionId: id,
        error: error instanceof Error ? error.message : String(error),
      },
      { status: 200 },
    );
  }
}
