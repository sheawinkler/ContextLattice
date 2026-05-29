import { NextResponse } from "next/server";
import { callOrchestrator } from "@/lib/orchestrator";

export async function GET(request: Request) {
  const url = new URL(request.url);
  const params = new URLSearchParams();
  for (const key of ["project", "limit", "include_ephemeral"]) {
    const value = url.searchParams.get(key);
    if (value) {
      params.set(key, value);
    }
  }
  const suffix = params.toString() ? `?${params.toString()}` : "";
  const data = await callOrchestrator(`/telemetry/memory/graph${suffix}`);
  return NextResponse.json(data);
}
