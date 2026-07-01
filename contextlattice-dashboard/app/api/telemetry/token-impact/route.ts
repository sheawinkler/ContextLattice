import { NextResponse } from "next/server";
import { callOrchestrator } from "@/lib/orchestrator";

export async function GET() {
  const data = await callOrchestrator("/telemetry/token-impact");
  return NextResponse.json(data);
}
