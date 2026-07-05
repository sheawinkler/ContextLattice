import { NextResponse } from "next/server";
import { callOrchestrator } from "@/lib/orchestrator";

export async function GET(request: Request) {
  const url = new URL(request.url);
  const params = new URLSearchParams();
  const limit = url.searchParams.get("limit");
  const taskClass = url.searchParams.get("task_class");
  if (limit) params.set("limit", limit);
  if (taskClass) params.set("task_class", taskClass);
  const suffix = params.toString() ? `?${params.toString()}` : "";
  const data = await callOrchestrator(`/telemetry/runner-quality${suffix}`);
  return NextResponse.json(data);
}
