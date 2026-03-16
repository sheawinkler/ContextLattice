import { NextRequest, NextResponse } from "next/server";
import { fetchOrchestrator } from "@/lib/orchestrator";

export const runtime = "nodejs";

export async function GET(
  req: NextRequest,
  { params }: { params: { token: string } },
) {
  const token = String(params.token || "").trim();
  if (!token) {
    return NextResponse.json({ error: "token is required" }, { status: 422 });
  }

  const qs = req.nextUrl.searchParams.toString();
  const path = `/memory/search/continuations/${encodeURIComponent(token)}/events${
    qs ? `?${qs}` : ""
  }`;

  const upstream = await fetchOrchestrator(path, {
    method: "GET",
    headers: {
      accept: "text/event-stream",
      "cache-control": "no-cache",
    },
  });

  if (!upstream.ok || !upstream.body) {
    const detail = await upstream.text();
    return NextResponse.json(
      {
        error: "continuation stream unavailable",
        status: upstream.status,
        detail,
      },
      { status: upstream.status || 502 },
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
