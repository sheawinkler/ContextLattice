import { NextResponse } from "next/server";
import { callOrchestrator } from "@/lib/orchestrator";

export async function GET() {
  const [snapshot, history] = await Promise.all([
    callOrchestrator("/telemetry/strategies"),
    callOrchestrator("/telemetry/strategies/history?limit=50"),
  ]);
  return NextResponse.json({ ...snapshot, history: history?.history ?? [] });
}
