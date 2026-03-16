import { NextResponse } from "next/server";
import { callOrchestrator } from "@/lib/orchestrator";

export async function POST(req: Request) {
  let payload: Record<string, unknown> = {};
  try {
    payload = await req.json();
  } catch {
    payload = {};
  }

  const data = await callOrchestrator("/memory/search", {
    method: "POST",
    body: JSON.stringify(payload),
  });
  return NextResponse.json(data);
}
