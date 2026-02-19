import { callOrchestrator } from "@/lib/orchestrator";
import { NextRequest, NextResponse } from "next/server";

export async function GET(req: NextRequest) {
  const { searchParams } = new URL(req.url);
  const project = searchParams.get("project");
  const depth = searchParams.get("depth");

  const params = new URLSearchParams();
  if (project) params.set("project", project);
  if (depth) params.set("depth", depth);

  const path = params.toString() ? `/memory/topics?${params.toString()}` : "/memory/topics";
  const data = await callOrchestrator(path);
  return NextResponse.json(data);
}
