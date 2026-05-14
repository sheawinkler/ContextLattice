import { NextRequest, NextResponse } from "next/server";
import { fetchOrchestrator } from "@/lib/orchestrator";

export const runtime = "nodejs";

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
  const candidates = [
    `/memory/search/continuations/${encodeURIComponent(token)}/events${
      qs ? `?${qs}` : ""
    }`,
    `/memory/search/jobs/${encodeURIComponent(token)}/events${
      qs ? `?${qs}` : ""
    }`,
  ];
  let upstream: Response | null = null;
  let detail = "";
  for (const path of candidates) {
    const response = await fetchOrchestrator(path, {
      method: "GET",
      headers: {
        accept: "text/event-stream",
        "cache-control": "no-cache",
      },
    });
    if (response.ok && response.body) {
      upstream = response;
      break;
    }
    detail = await response.text();
  }

  if (!upstream || !upstream.ok || !upstream.body) {
    return NextResponse.json(
      {
        error: "continuation stream unavailable",
        status: upstream?.status ?? 502,
        detail,
      },
      { status: upstream?.status || 502 },
    );
  }

  const headers = new Headers();
  headers.set("content-type", "text/event-stream");
  headers.set("cache-control", "no-cache, no-transform");
  headers.set("connection", "keep-alive");

  return new Response(upstream.body, {
    status: upstream.status,
    headers,
  });
}
