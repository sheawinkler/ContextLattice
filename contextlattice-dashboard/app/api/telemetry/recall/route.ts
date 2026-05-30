import { NextResponse } from "next/server";
import { callOrchestrator } from "@/lib/orchestrator";

export async function GET(request: Request) {
  const url = new URL(request.url);
  const params = new URLSearchParams();
  for (const key of ["traffic_class"]) {
    const value = url.searchParams.get(key);
    if (value) {
      params.set(key, value);
    }
  }
  const suffix = params.toString() ? `?${params.toString()}` : "";
  const data = await callOrchestrator(`/telemetry/recall${suffix}`);
  return NextResponse.json(data);
}
