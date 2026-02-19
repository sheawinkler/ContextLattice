import { callOrchestrator } from "@/lib/orchestrator";
import { NextRequest, NextResponse } from "next/server";

export async function GET(req: NextRequest) {
  const { searchParams } = new URL(req.url);
  const project = searchParams.get("project");
  const userId = searchParams.get("user_id");
  const limit = searchParams.get("limit");

  const params = new URLSearchParams();
  if (project) params.set("project", project);
  if (userId) params.set("user_id", userId);
  if (limit) params.set("limit", limit);

  const path = params.toString() ? `/preferences?${params.toString()}` : "/preferences";
  const data = await callOrchestrator(path);
  return NextResponse.json(data);
}
