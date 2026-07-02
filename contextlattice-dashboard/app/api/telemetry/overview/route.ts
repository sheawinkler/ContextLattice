import { NextResponse } from "next/server";
import { fetchOrchestrator } from "@/lib/orchestrator";

type SafeResponse = {
  data: any | null;
  error: string | null;
  fetchedAt: string;
};

function clampInt(value: string | null, fallback: number, min: number, max: number): number {
  const parsed = Number.parseInt(String(value ?? ""), 10);
  if (!Number.isFinite(parsed)) {
    return fallback;
  }
  return Math.max(min, Math.min(max, parsed));
}

async function safeGet(
  path: string,
  options?: { optionalStatuses?: number[] },
): Promise<SafeResponse> {
  const fetchedAt = new Date().toISOString();
  try {
    const response = await fetchOrchestrator(path, { method: "GET" });
    if (!response.ok) {
      if (options?.optionalStatuses?.includes(response.status)) {
        return {
          data: null,
          error: null,
          fetchedAt,
        };
      }
      const detail = (await response.text()).slice(0, 280);
      return {
        data: null,
        error: `${path} -> ${response.status}${detail ? ` ${detail}` : ""}`,
        fetchedAt,
      };
    }
    return {
      data: await response.json(),
      error: null,
      fetchedAt,
    };
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    return { data: null, error: `${path} -> ${message}`, fetchedAt };
  }
}

function compactToolItems(payload: any): any {
  if (!payload || typeof payload !== "object") {
    return payload;
  }
  const items = Array.isArray(payload.items) ? payload.items : [];
  return {
    ...payload,
    items: items.map((item: any) => ({
      id: item?.id,
      timestamp: item?.timestamp,
      path: item?.path,
      tool: item?.tool,
      status_code: item?.status_code,
      duration_ms: item?.duration_ms,
      project: item?.project,
      agent_id: item?.agent_id,
      error: item?.error,
    })),
  };
}

export async function GET(request: Request) {
  const url = new URL(request.url);
  const recallHistory = clampInt(url.searchParams.get("recallHistory"), 32, 8, 128);
  const toolLimit = clampInt(url.searchParams.get("toolLimit"), 20, 5, 100);

  const [
    status,
    telemetryMetrics,
    tokenImpact,
    contextPackQuality,
    memoryTelemetry,
    retrieval,
    fanout,
    storage,
    sidecarHealth,
    strategyTelemetry,
    strategyHistory,
    tradingTelemetry,
    tradingHistory,
    recall,
    recallMonitor,
    queueStatus,
    agentRuntime,
    nativeOwnership,
    tools,
    sentrux,
  ] = await Promise.all([
    safeGet("/status"),
    safeGet("/telemetry/metrics"),
    safeGet("/telemetry/token-impact"),
    safeGet("/telemetry/context-pack-quality"),
    safeGet("/telemetry/memory"),
    safeGet("/telemetry/retrieval?traffic_class=user"),
    safeGet("/telemetry/fanout"),
    safeGet("/telemetry/storage"),
    safeGet("/telemetry/sidecar-health"),
    safeGet("/telemetry/strategies"),
    safeGet("/telemetry/strategies/history?limit=50"),
    safeGet("/telemetry/trading"),
    safeGet("/telemetry/trading/history?limit=50"),
    safeGet("/telemetry/recall"),
    safeGet("/telemetry/recall/monitor"),
    safeGet("/ops/queue/status?include_deadletters=false"),
    safeGet("/telemetry/agents/runtime?limit=16"),
    safeGet("/ops/native-ownership"),
    safeGet(`/telemetry/tools/invocations?limit=${toolLimit}&status_min=400`),
    safeGet("/ops/quality/sentrux/status", { optionalStatuses: [404] }),
  ]);

  const errors: Record<string, string> = {};
  const endpoints = {
    status,
    telemetryMetrics,
    tokenImpact,
    contextPackQuality,
    memoryTelemetry,
    retrieval,
    fanout,
    storage,
    sidecarHealth,
    strategyTelemetry,
    strategyHistory,
    tradingTelemetry,
    tradingHistory,
    recall,
    recallMonitor,
    queueStatus,
    agentRuntime,
    nativeOwnership,
    tools,
    sentrux,
  } as const;

  Object.entries(endpoints).forEach(([key, value]) => {
    if (value.error) {
      errors[key] = value.error;
    }
  });
  const freshness = Object.fromEntries(
    Object.entries(endpoints).map(([key, value]) => [key, value.fetchedAt]),
  );

  const recallMonitorPayload =
    recallMonitor.data && typeof recallMonitor.data === "object"
      ? {
          ...recallMonitor.data,
          history: Array.isArray(recallMonitor.data.history)
            ? recallMonitor.data.history.slice(0, recallHistory)
            : [],
        }
      : null;

  return NextResponse.json({
    capturedAt: new Date().toISOString(),
    status: status.data,
    telemetryMetrics: telemetryMetrics.data,
    tokenImpact: tokenImpact.data,
    contextPackQuality: contextPackQuality.data,
    memoryTelemetry: memoryTelemetry.data,
    retrieval: retrieval.data,
    fanout: fanout.data,
    storage: storage.data,
    sidecarHealth: sidecarHealth.data,
    strategyTelemetry: strategyTelemetry.data,
    strategyHistory:
      strategyHistory.data && typeof strategyHistory.data === "object"
        ? Array.isArray(strategyHistory.data.history)
          ? strategyHistory.data.history
          : []
        : [],
    tradingTelemetry: tradingTelemetry.data,
    tradingHistory:
      tradingHistory.data && typeof tradingHistory.data === "object"
        ? Array.isArray(tradingHistory.data.history)
          ? tradingHistory.data.history
          : []
        : [],
    recall: recall.data,
    recallMonitor: recallMonitorPayload,
    queueStatus: queueStatus.data,
    agentRuntime: agentRuntime.data,
    nativeOwnership: nativeOwnership.data,
    tools: compactToolItems(tools.data),
    sentrux: sentrux.data,
    freshness,
    errors,
  });
}
