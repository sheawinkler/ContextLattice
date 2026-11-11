import { NextResponse } from "next/server";
import { fetchOrchestrator } from "@/lib/orchestrator";

type SafeResponse = {
  data: any | null;
  error: string | null;
  attemptedAt: string;
  succeededAt: string | null;
  sourceAt: string | null;
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
  signal: AbortSignal,
  options?: { optionalStatuses?: number[] },
): Promise<SafeResponse> {
  const attemptedAt = new Date().toISOString();
  const unknown = { attemptedAt, succeededAt: null, sourceAt: null };
  try {
    const response = await fetchOrchestrator(path, { method: "GET", signal });
    if (!response.ok) {
      if (options?.optionalStatuses?.includes(response.status)) {
        return {
          data: null,
          error: null,
          ...unknown,
        };
      }
      const detail = (await response.text()).slice(0, 280);
      return {
        data: null,
        error: `${path} -> ${response.status}${detail ? ` ${detail}` : ""}`,
        ...unknown,
      };
    }
    const data = await response.json();
    const source = data?.updatedAt ?? data?.updated_at ?? data?.generatedAt ?? data?.capturedAt;
    return {
      data,
      error: null,
      attemptedAt,
      succeededAt: new Date().toISOString(),
      sourceAt: typeof source === "string" && Number.isFinite(Date.parse(source)) ? source : null,
    };
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    return { data: null, error: `${path} -> ${message}`, ...unknown };
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
  const memoryOnly = url.searchParams.get("view") === "memory";
  const signal = AbortSignal.any([request.signal, AbortSignal.timeout(8000)]);
  const get = (path: string) => safeGet(path, signal);
  const omitted: SafeResponse = { data: null, error: null, attemptedAt: "", succeededAt: null, sourceAt: null };
  const specialist = (path: string) => memoryOnly ? Promise.resolve(omitted) : get(path);

  const [
    status,
    telemetryMetrics,
    tokenImpact,
    contextPackQuality,
    runnerQuality,
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
    get("/status"),
    specialist("/telemetry/metrics"),
    get("/telemetry/token-impact"),
    get("/telemetry/context-pack-quality"),
    get("/telemetry/runner-quality"),
    get("/telemetry/memory"),
    get("/telemetry/retrieval?traffic_class=user"),
    specialist("/telemetry/fanout"),
    get("/telemetry/storage"),
    specialist("/telemetry/sidecar-health"),
    specialist("/telemetry/strategies"),
    specialist("/telemetry/strategies/history?limit=50"),
    specialist("/telemetry/trading"),
    specialist("/telemetry/trading/history?limit=50"),
    specialist("/telemetry/recall"),
    specialist("/telemetry/recall/monitor"),
    specialist("/ops/queue/status?include_deadletters=false"),
    get("/telemetry/agents/runtime?limit=16"),
    specialist("/ops/native-ownership"),
    specialist(`/telemetry/tools/invocations?limit=${toolLimit}&status_min=400`),
    memoryOnly ? omitted : safeGet("/ops/quality/sentrux/status", signal, { optionalStatuses: [404] }),
  ]);

  const errors: Record<string, string> = {};
  const endpoints = {
    status,
    telemetryMetrics,
    tokenImpact,
    contextPackQuality,
    runnerQuality,
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
    Object.entries(endpoints).map(([key, value]) => [key, value.succeededAt]),
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
    snapshotId: crypto.randomUUID(),
    snapshotAtomic: false,
    view: memoryOnly ? "memory" : "full",
    sourceFreshness: Object.fromEntries(Object.entries(endpoints).filter(([, value]) => value !== omitted).map(([key, value]) => [key, {
      attemptedAt: value.attemptedAt, succeededAt: value.succeededAt, sourceAt: value.sourceAt,
    }])),
    capturedAt: new Date().toISOString(),
    status: status.data,
    telemetryMetrics: telemetryMetrics.data,
    tokenImpact: tokenImpact.data,
    contextPackQuality: contextPackQuality.data,
    runnerQuality: runnerQuality.data,
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
