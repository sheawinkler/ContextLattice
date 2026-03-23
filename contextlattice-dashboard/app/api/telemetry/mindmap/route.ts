import { NextResponse } from "next/server";
import { fetchOrchestrator } from "@/lib/orchestrator";

type SourceRow = {
  source: string;
  lane: "fast" | "slow";
  requests: number;
  errors: number;
  timeouts: number;
  p95Ms: number;
  p99Ms: number;
  avgMs: number;
};

const FAST_SOURCES = new Set(["qdrant", "postgres_pgvector", "topic_rollups"]);

function toNumber(value: unknown): number {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : 0;
}

function toInt(value: unknown): number {
  return Math.max(0, Math.round(toNumber(value)));
}

function laneForSource(source: string): "fast" | "slow" {
  return FAST_SOURCES.has(source) ? "fast" : "slow";
}

export async function GET() {
  try {
    const response = await fetchOrchestrator("/telemetry/retrieval?traffic_class=user", {
      method: "GET",
    });
    if (!response.ok) {
      const detail = (await response.text()).slice(0, 300);
      return NextResponse.json(
        {
          capturedAt: new Date().toISOString(),
          sources: [],
          summary: {
            totalSources: 0,
            totalRequests: 0,
            totalFailures: 0,
          },
          error: `/telemetry/retrieval -> ${response.status}${detail ? ` ${detail}` : ""}`,
        },
        { status: 200 },
      );
    }

    const payload = await response.json();
    const sourceMetrics = (payload?.latency?.sources ?? {}) as Record<string, Record<string, unknown>>;
    const sources: SourceRow[] = Object.entries(sourceMetrics)
      .map(([source, metrics]) => ({
        source,
        lane: laneForSource(source),
        requests: toInt(metrics?.requests),
        errors: toInt(metrics?.errors),
        timeouts: toInt(metrics?.timeouts),
        p95Ms: toNumber(metrics?.p95Ms),
        p99Ms: toNumber(metrics?.p99Ms),
        avgMs: toNumber(metrics?.avgMs),
      }))
      .sort((a, b) => b.requests - a.requests || b.p95Ms - a.p95Ms || a.source.localeCompare(b.source));

    const summary = sources.reduce(
      (acc, row) => {
        acc.totalRequests += row.requests;
        acc.totalFailures += row.errors + row.timeouts;
        return acc;
      },
      {
        totalSources: sources.length,
        totalRequests: 0,
        totalFailures: 0,
      },
    );

    return NextResponse.json({
      capturedAt: new Date().toISOString(),
      retrievalUpdatedAt: payload?.latency?.updatedAt ?? null,
      defaultMode: payload?.defaultMode ?? "balanced",
      trafficClass: payload?.trafficClass ?? "user",
      sourceCircuit: payload?.sourceCircuit ?? null,
      sources,
      summary,
    });
  } catch (error) {
    return NextResponse.json(
      {
        capturedAt: new Date().toISOString(),
        sources: [],
        summary: {
          totalSources: 0,
          totalRequests: 0,
          totalFailures: 0,
        },
        error: error instanceof Error ? error.message : String(error),
      },
      { status: 200 },
    );
  }
}
