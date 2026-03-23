"use client";

import { useCallback, useEffect, useMemo, useState } from "react";

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

type MindMapPayload = {
  capturedAt?: string;
  retrievalUpdatedAt?: string | null;
  defaultMode?: string;
  trafficClass?: string;
  sources?: SourceRow[];
  summary?: {
    totalSources?: number;
    totalRequests?: number;
    totalFailures?: number;
  };
  error?: string;
};

type PositionedSource = SourceRow & {
  angle: number;
  sourceX: number;
  sourceY: number;
  latencyX: number;
  latencyY: number;
  healthX: number;
  healthY: number;
  state: "ok" | "warn" | "error";
};

const REFRESH_MS = 12_000;
const VIEWBOX_W = 980;
const VIEWBOX_H = 560;
const CENTER_X = 490;
const CENTER_Y = 280;
const SOURCE_RING_RADIUS = 190;
const METRIC_RING_RADIUS = 300;

function formatMs(value: number): string {
  if (!Number.isFinite(value) || value <= 0) {
    return "--";
  }
  if (value < 1000) {
    return `${value.toFixed(0)}ms`;
  }
  return `${(value / 1000).toFixed(2)}s`;
}

function formatCount(value: number): string {
  if (!Number.isFinite(value)) {
    return "--";
  }
  return new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 }).format(value);
}

function sourceState(row: SourceRow): "ok" | "warn" | "error" {
  const failures = row.errors + row.timeouts;
  if (row.timeouts > 0 || failures >= 3 || row.p95Ms >= 5000) {
    return "error";
  }
  if (failures > 0 || row.p95Ms >= 1800) {
    return "warn";
  }
  return "ok";
}

function sourceChipClass(active: boolean): string {
  return active ? "ops-mindmap-chip ops-mindmap-chip-active" : "ops-mindmap-chip";
}

export function SourceMindMap() {
  const [payload, setPayload] = useState<MindMapPayload | null>(null);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selectedSources, setSelectedSources] = useState<string[]>([]);
  const [focusedSource, setFocusedSource] = useState<string | null>(null);

  const load = useCallback(async (initial = false) => {
    if (initial) {
      setLoading(true);
    } else {
      setRefreshing(true);
    }
    try {
      setError(null);
      const response = await fetch("/api/telemetry/mindmap", { cache: "no-store" });
      const data = (await response.json()) as MindMapPayload;
      if (!response.ok) {
        throw new Error(typeof data === "object" ? JSON.stringify(data) : "mindmap load failed");
      }
      setPayload(data);
      if (typeof data.error === "string" && data.error.trim()) {
        setError(data.error.trim());
      }
    } catch (loadError) {
      const message = loadError instanceof Error ? loadError.message : "mindmap load failed";
      setError(message);
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    void load(true);
    const timer = window.setInterval(() => {
      void load(false);
    }, REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [load]);

  const sources = useMemo(() => payload?.sources ?? [], [payload?.sources]);

  useEffect(() => {
    if (sources.length === 0) {
      setSelectedSources([]);
      setFocusedSource(null);
      return;
    }
    const available = new Set(sources.map((row) => row.source));
    setSelectedSources((prev) => {
      if (prev.length === 0) {
        return sources.map((row) => row.source);
      }
      const next = prev.filter((source) => available.has(source));
      return next.length > 0 ? next : sources.map((row) => row.source);
    });
    setFocusedSource((prev) => (prev && available.has(prev) ? prev : sources[0].source));
  }, [sources]);

  const selectedSet = useMemo(() => new Set(selectedSources), [selectedSources]);

  const visibleSources = useMemo(() => {
    return sources.filter((row) => selectedSet.has(row.source));
  }, [sources, selectedSet]);

  const positionedSources = useMemo<PositionedSource[]>(() => {
    const rows = visibleSources;
    if (rows.length === 0) {
      return [];
    }
    const step = (2 * Math.PI) / rows.length;
    return rows.map((row, index) => {
      const angle = -Math.PI / 2 + step * index;
      const sourceX = CENTER_X + Math.cos(angle) * SOURCE_RING_RADIUS;
      const sourceY = CENTER_Y + Math.sin(angle) * SOURCE_RING_RADIUS;
      const latencyAngle = angle - 0.16;
      const healthAngle = angle + 0.16;
      return {
        ...row,
        angle,
        sourceX,
        sourceY,
        latencyX: CENTER_X + Math.cos(latencyAngle) * METRIC_RING_RADIUS,
        latencyY: CENTER_Y + Math.sin(latencyAngle) * METRIC_RING_RADIUS,
        healthX: CENTER_X + Math.cos(healthAngle) * METRIC_RING_RADIUS,
        healthY: CENTER_Y + Math.sin(healthAngle) * METRIC_RING_RADIUS,
        state: sourceState(row),
      };
    });
  }, [visibleSources]);

  const focused = useMemo(() => {
    if (positionedSources.length === 0) {
      return null;
    }
    return positionedSources.find((row) => row.source === focusedSource) ?? positionedSources[0];
  }, [focusedSource, positionedSources]);

  const toggleSource = useCallback((source: string) => {
    setSelectedSources((prev) => {
      if (prev.includes(source)) {
        if (prev.length === 1) {
          return prev;
        }
        return prev.filter((item) => item !== source);
      }
      return [...prev, source];
    });
    setFocusedSource(source);
  }, []);

  const selectLane = useCallback(
    (lane: "all" | "fast" | "slow") => {
      if (lane === "all") {
        setSelectedSources(sources.map((row) => row.source));
        return;
      }
      const laneSources = sources.filter((row) => row.lane === lane).map((row) => row.source);
      if (laneSources.length > 0) {
        setSelectedSources(laneSources);
        setFocusedSource(laneSources[0]);
      }
    },
    [sources],
  );

  return (
    <div className="ops-shell">
      <section className="ops-card">
        <div className="ops-mindmap-head">
          <div>
            <h3 className="ops-card-title">Source Mindmap</h3>
            <p className="ops-card-text">
              Obsidian-style source graph with selectable DB lanes and live retrieval pressure links.
            </p>
          </div>
          <button
            type="button"
            className="ops-refresh"
            onClick={() => void load(false)}
            disabled={refreshing}
          >
            {refreshing ? "Refreshing..." : "Refresh"}
          </button>
        </div>

        <div className="ops-mindmap-toolbar">
          <div className="ops-mindmap-chip-row">
            <button
              type="button"
              className={sourceChipClass(selectedSources.length === sources.length && sources.length > 0)}
              onClick={() => selectLane("all")}
            >
              All
            </button>
            <button
              type="button"
              className={sourceChipClass(
                selectedSources.length > 0 && selectedSources.every((source) => {
                  const row = sources.find((item) => item.source === source);
                  return row?.lane === "fast";
                }),
              )}
              onClick={() => selectLane("fast")}
            >
              Fast Lane
            </button>
            <button
              type="button"
              className={sourceChipClass(
                selectedSources.length > 0 && selectedSources.every((source) => {
                  const row = sources.find((item) => item.source === source);
                  return row?.lane === "slow";
                }),
              )}
              onClick={() => selectLane("slow")}
            >
              Slow Lane
            </button>
          </div>
          <div className="ops-mindmap-chip-row">
            {sources.map((row) => (
              <button
                key={row.source}
                type="button"
                className={sourceChipClass(selectedSet.has(row.source))}
                onClick={() => toggleSource(row.source)}
                title={`${row.source} · ${row.lane} · p95 ${formatMs(row.p95Ms)}`}
              >
                {row.source}
              </button>
            ))}
          </div>
        </div>

        {error && <div className="ops-mindmap-error">{error}</div>}

        {loading ? (
          <div className="ops-empty">Loading mindmap telemetry…</div>
        ) : (
          <div className="ops-mindmap-canvas-wrap">
            <svg className="ops-mindmap-canvas" viewBox={`0 0 ${VIEWBOX_W} ${VIEWBOX_H}`} role="img" aria-label="source mindmap">
              <defs>
                <radialGradient id="opsMindCore" cx="50%" cy="45%" r="60%">
                  <stop offset="0%" stopColor="rgba(34, 211, 238, 0.35)" />
                  <stop offset="100%" stopColor="rgba(15, 23, 42, 0.75)" />
                </radialGradient>
                <radialGradient id="opsMindBg" cx="50%" cy="50%" r="70%">
                  <stop offset="0%" stopColor="rgba(30, 41, 59, 0.32)" />
                  <stop offset="100%" stopColor="rgba(2, 6, 23, 0.94)" />
                </radialGradient>
              </defs>
              <rect x="0" y="0" width={VIEWBOX_W} height={VIEWBOX_H} fill="url(#opsMindBg)" rx="18" />
              <circle cx={CENTER_X} cy={CENTER_Y} r="84" className="ops-mindmap-core-node" fill="url(#opsMindCore)" />
              <text x={CENTER_X} y={CENTER_Y - 6} className="ops-mindmap-core-label" textAnchor="middle">
                ContextLattice
              </text>
              <text x={CENTER_X} y={CENTER_Y + 18} className="ops-mindmap-core-sub" textAnchor="middle">
                Retrieval Core
              </text>

              {positionedSources.map((row) => (
                <g key={`${row.source}-edges`}>
                  <line
                    x1={CENTER_X}
                    y1={CENTER_Y}
                    x2={row.sourceX}
                    y2={row.sourceY}
                    className={`ops-mindmap-edge ops-mindmap-edge-${row.state}`}
                  />
                  <line
                    x1={row.sourceX}
                    y1={row.sourceY}
                    x2={row.latencyX}
                    y2={row.latencyY}
                    className="ops-mindmap-edge ops-mindmap-edge-metric"
                  />
                  <line
                    x1={row.sourceX}
                    y1={row.sourceY}
                    x2={row.healthX}
                    y2={row.healthY}
                    className="ops-mindmap-edge ops-mindmap-edge-metric"
                  />
                </g>
              ))}

              {positionedSources.map((row) => (
                <g
                  key={row.source}
                  className={`ops-mindmap-node-group ${focused?.source === row.source ? "ops-mindmap-node-focused" : ""}`}
                  onClick={() => setFocusedSource(row.source)}
                >
                  <circle
                    cx={row.sourceX}
                    cy={row.sourceY}
                    r="28"
                    className={`ops-mindmap-source-node ops-mindmap-source-${row.state}`}
                  />
                  <text x={row.sourceX} y={row.sourceY + 4} className="ops-mindmap-source-label" textAnchor="middle">
                    {row.source}
                  </text>
                  <title>{`${row.source} | ${row.lane} | req=${row.requests} p95=${formatMs(row.p95Ms)} p99=${formatMs(row.p99Ms)} err=${row.errors} timeout=${row.timeouts}`}</title>
                </g>
              ))}

              {positionedSources.map((row) => (
                <g key={`${row.source}-latency`} className="ops-mindmap-metric-node-group">
                  <circle cx={row.latencyX} cy={row.latencyY} r="18" className="ops-mindmap-metric-node" />
                  <text x={row.latencyX} y={row.latencyY + 4} className="ops-mindmap-metric-label" textAnchor="middle">
                    {formatMs(row.p95Ms)}
                  </text>
                  <title>{`${row.source} latency p95=${formatMs(row.p95Ms)} p99=${formatMs(row.p99Ms)} avg=${formatMs(row.avgMs)}`}</title>
                </g>
              ))}

              {positionedSources.map((row) => (
                <g key={`${row.source}-health`} className="ops-mindmap-metric-node-group">
                  <circle cx={row.healthX} cy={row.healthY} r="18" className="ops-mindmap-metric-node" />
                  <text x={row.healthX} y={row.healthY + 4} className="ops-mindmap-metric-label" textAnchor="middle">
                    {formatCount(row.errors + row.timeouts)}
                  </text>
                  <title>{`${row.source} reliability failures=${row.errors + row.timeouts} (errors=${row.errors}, timeouts=${row.timeouts})`}</title>
                </g>
              ))}
            </svg>
          </div>
        )}

        <div className="ops-grid ops-grid-2">
          <div className="ops-card ops-mindmap-detail">
            <h4 className="ops-card-title">Selected Source</h4>
            {focused ? (
              <div className="ops-runtime-grid">
                <div className="ops-runtime-item">
                  <span className="ops-runtime-label">Source</span>
                  <span className="ops-runtime-value">{focused.source}</span>
                </div>
                <div className="ops-runtime-item">
                  <span className="ops-runtime-label">Lane</span>
                  <span className="ops-runtime-value">{focused.lane}</span>
                </div>
                <div className="ops-runtime-item">
                  <span className="ops-runtime-label">Requests</span>
                  <span className="ops-runtime-value">{formatCount(focused.requests)}</span>
                </div>
                <div className="ops-runtime-item">
                  <span className="ops-runtime-label">P95</span>
                  <span className="ops-runtime-value">{formatMs(focused.p95Ms)}</span>
                </div>
                <div className="ops-runtime-item">
                  <span className="ops-runtime-label">P99</span>
                  <span className="ops-runtime-value">{formatMs(focused.p99Ms)}</span>
                </div>
                <div className="ops-runtime-item">
                  <span className="ops-runtime-label">Failures</span>
                  <span className="ops-runtime-value">{formatCount(focused.errors + focused.timeouts)}</span>
                </div>
              </div>
            ) : (
              <div className="ops-empty">Select a source node to inspect details.</div>
            )}
          </div>

          <div className="ops-card ops-mindmap-detail">
            <h4 className="ops-card-title">Graph Summary</h4>
            <div className="ops-runtime-grid">
              <div className="ops-runtime-item">
                <span className="ops-runtime-label">Visible Sources</span>
                <span className="ops-runtime-value">{formatCount(visibleSources.length)}</span>
              </div>
              <div className="ops-runtime-item">
                <span className="ops-runtime-label">Total Requests</span>
                <span className="ops-runtime-value">{formatCount(payload?.summary?.totalRequests ?? 0)}</span>
              </div>
              <div className="ops-runtime-item">
                <span className="ops-runtime-label">Total Failures</span>
                <span className="ops-runtime-value">{formatCount(payload?.summary?.totalFailures ?? 0)}</span>
              </div>
              <div className="ops-runtime-item">
                <span className="ops-runtime-label">Retrieval Mode</span>
                <span className="ops-runtime-value">{payload?.defaultMode || "--"}</span>
              </div>
            </div>
            <p className="ops-card-text">
              Snapshot: {payload?.capturedAt ? new Date(payload.capturedAt).toLocaleTimeString() : "--"} ·
              retrieval: {payload?.retrievalUpdatedAt ? new Date(payload.retrievalUpdatedAt).toLocaleTimeString() : "--"}
            </p>
          </div>
        </div>
      </section>
    </div>
  );
}
