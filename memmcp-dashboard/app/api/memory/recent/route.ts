import { NextResponse } from "next/server";
import { callOrchestrator } from "@/lib/orchestrator";

export async function GET(req: Request) {
  const { searchParams } = new URL(req.url);
  const limit = searchParams.get("limit");
  const project = searchParams.get("project");
  const params = new URLSearchParams();
  if (limit) {
    params.set("limit", limit);
  }
  if (project) {
    params.set("project", project);
  }
  const path = params.toString()
    ? `/memory/recent?${params.toString()}`
    : "/memory/recent";
  const data = await callOrchestrator(path);
  return NextResponse.json(data);
}
