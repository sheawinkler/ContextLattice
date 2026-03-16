import { NextRequest, NextResponse } from "next/server";
import { callOrchestrator } from "@/lib/orchestrator";

export async function GET(req: NextRequest) {
  const memoryId = req.nextUrl.searchParams.get("memory_id")?.trim() ?? "";
  if (!memoryId) {
    return NextResponse.json({ error: "memory_id is required" }, { status: 422 });
  }
  const path = `/v1/memory/get?memory_id=${encodeURIComponent(memoryId)}`;
  const data = await callOrchestrator(path);
  return NextResponse.json(data);
}
