import { NextRequest, NextResponse } from "next/server";
import { callOrchestrator } from "@/lib/orchestrator";

export async function GET(
  req: NextRequest,
  { params }: { params: Promise<{ token: string }> },
) {
  const resolved = await params;
  const token = String(resolved.token || "").trim();
  if (!token) {
    return NextResponse.json({ error: "token is required" }, { status: 422 });
  }

  const qs = req.nextUrl.searchParams.toString();
  const suffix = qs ? `?${qs}` : "";
  const data = await callOrchestrator(
    `/memory/search/continuations/${encodeURIComponent(token)}${suffix}`,
    { method: "GET" },
  );
  return NextResponse.json(data);
}
