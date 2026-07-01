import { NextResponse } from "next/server";
import { fetchOrchestrator } from "@/lib/orchestrator";

export async function GET() {
  const startedAt = Date.now();
  try {
    const response = await fetchOrchestrator("/health");
    const elapsedMs = Date.now() - startedAt;
    if (!response.ok) {
      const detail = (await response.text()).slice(0, 280);
      return NextResponse.json(
        {
          ok: false,
          status: "degraded",
          orchestratorStatus: response.status,
          error: detail || "orchestrator health failed",
          elapsedMs,
        },
        { status: 200 },
      );
    }
    const payload = await response.json();
    return NextResponse.json(
      {
        ok: true,
        status: "healthy",
        elapsedMs,
        orchestrator: payload,
      },
      { status: 200 },
    );
  } catch (error) {
    const elapsedMs = Date.now() - startedAt;
    const message = error instanceof Error ? error.message : String(error);
    return NextResponse.json(
      {
        ok: false,
        status: "degraded",
        elapsedMs,
        error: message,
      },
      { status: 200 },
    );
  }
}
