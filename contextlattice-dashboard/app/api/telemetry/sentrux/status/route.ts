import { fetchOrchestrator } from "@/lib/orchestrator";

function proxyHeaders(response: Response) {
  return {
    "cache-control": "no-store",
    "content-type": response.headers.get("content-type") || "application/json",
  };
}

export async function GET() {
  try {
    const response = await fetchOrchestrator("/ops/quality/sentrux/status", {
      method: "GET",
    });
    const body = await response.text();
    return new Response(body, {
      status: response.status,
      headers: proxyHeaders(response),
    });
  } catch (error) {
    return Response.json(
      {
        ok: false,
        error: "sentrux_status_proxy_failed",
        detail: error instanceof Error ? error.message : String(error),
      },
      { status: 502, headers: { "cache-control": "no-store" } },
    );
  }
}
