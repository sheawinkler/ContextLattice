"use client";

type QualityTotals = {
  requests?: number;
  timeouts?: number;
  errors?: number;
  sourceErrorRate?: number;
  noHitRate?: number;
  lowConfidenceRate?: number;
  staleHitRate?: number;
  recallAtK?: number | null;
  mrr?: number | null;
  numericExactness?: number | null;
  citationCoverage?: number | null;
  sourceDiversity?: number | null;
  graphLift?: number | null;
  evalP95Ms?: number | null;
  lastEvalAt?: string | null;
};

export type RecallQualityPayload = {
  updatedAt?: string;
  trafficClass?: string;
  quality?: {
    status?: string;
    totals?: QualityTotals;
    sampleCount?: number;
    recommendations?: string[];
  };
  alerts?: {
    count?: number;
  };
};

export type RecallTuningPayload = {
  window?: {
    samples?: number;
    minSamples?: number;
    sufficient?: boolean;
  };
  recommended?: {
    quality?: {
      graphExpansion?: {
        enabled?: boolean;
        depth?: number;
        neighborLimit?: number;
      };
      sourceOrder?: string[];
      recommendations?: string[];
    };
  };
};

function numberValue(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function percentText(value: unknown): string {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    return "-";
  }
  return `${Math.round(value * 100)}%`;
}

function statusTone(status: string) {
  if (status === "repair_recommended" || status === "insufficient_cases") {
    return "bg-amber-500 text-amber-950";
  }
  if (status === "watch" || status === "unknown") {
    return "bg-cyan-500 text-cyan-950";
  }
  return "bg-emerald-500 text-emerald-950";
}

function QualityBar({ value, tone = "good" }: { value: number; tone?: "good" | "warn" | "neutral" }) {
  const pct = Math.max(0, Math.min(100, value * 100));
  const color = tone === "warn" ? "bg-amber-300" : tone === "neutral" ? "bg-cyan-300" : "bg-emerald-300";
  return (
    <div className="h-2 w-full rounded bg-slate-800 overflow-hidden" aria-hidden="true">
      <div className={`h-full ${color}`} style={{ width: `${pct}%` }} />
    </div>
  );
}

function Metric({ label, value, tone }: { label: string; value: string; tone?: "warn" | "good" }) {
  const toneClass =
    tone === "warn" ? "text-amber-200 border-amber-500/70" : tone === "good" ? "text-emerald-200 border-emerald-500/70" : "text-slate-200 border-slate-600";
  return (
    <div className={`rounded border px-3 py-2 ${toneClass}`}>
      <div className="text-xs uppercase tracking-wide text-slate-400">{label}</div>
      <div className="text-lg font-semibold">{value}</div>
    </div>
  );
}

export function RecallQualityPanel({
  recall,
  tuning,
}: {
  recall: RecallQualityPayload | null;
  tuning?: RecallTuningPayload | null;
}) {
  if (!recall) {
    return (
      <section className="card">
        <h3 className="text-lg font-semibold">Recall quality</h3>
        <p className="text-sm text-slate-400 mt-2">Recall telemetry unavailable.</p>
      </section>
    );
  }

  const totals = recall.quality?.totals ?? {};
  const status = String(recall.quality?.status || "unknown");
  const graphExpansion = tuning?.recommended?.quality?.graphExpansion;
  const sourceOrder = tuning?.recommended?.quality?.sourceOrder ?? [];
  const recallAtK = typeof totals.recallAtK === "number" ? totals.recallAtK : 0;
  const mrr = typeof totals.mrr === "number" ? totals.mrr : 0;
  const citationCoverage = typeof totals.citationCoverage === "number" ? totals.citationCoverage : 0;
  const graphLift = typeof totals.graphLift === "number" ? totals.graphLift : 0;
  const recommendations = [
    ...(recall.quality?.recommendations ?? []),
    ...(tuning?.recommended?.quality?.recommendations ?? []),
  ].slice(0, 4);

  return (
    <section className="card space-y-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 className="text-lg font-semibold">Recall quality</h3>
          <p className="text-xs text-slate-500 mt-1">
            {totals.lastEvalAt ? `Last eval ${new Date(totals.lastEvalAt).toLocaleTimeString()}` : "No saved eval sample yet"}
          </p>
        </div>
        <span className={`text-xs px-2 py-1 rounded ${statusTone(status)}`}>{status}</span>
      </div>

      <div className="grid md:grid-cols-6 gap-3 text-sm">
        <Metric label="Recall@K" value={percentText(totals.recallAtK)} tone={recallAtK >= 0.75 ? "good" : "warn"} />
        <Metric label="MRR" value={mrr ? mrr.toFixed(2) : "-"} tone={mrr >= 0.55 ? "good" : "warn"} />
        <Metric label="Citations" value={percentText(totals.citationCoverage)} tone={citationCoverage >= 0.9 ? "good" : "warn"} />
        <Metric label="Graph lift" value={percentText(totals.graphLift)} tone={graphLift > 0 ? "good" : undefined} />
        <Metric label="Diversity" value={numberValue(totals.sourceDiversity).toFixed(1)} />
        <Metric label="Eval p95" value={totals.evalP95Ms ? `${Math.round(totals.evalP95Ms)} ms` : "-"} />
      </div>

      <div className="grid lg:grid-cols-[minmax(0,1fr)_18rem] gap-5">
        <div className="space-y-3">
          <div className="grid grid-cols-[5rem_minmax(0,1fr)_3.5rem] items-center gap-3 text-xs">
            <span className="text-slate-400">recall</span>
            <QualityBar value={recallAtK} tone={recallAtK >= 0.75 ? "good" : "warn"} />
            <span className="text-right text-slate-300">{percentText(totals.recallAtK)}</span>
          </div>
          <div className="grid grid-cols-[5rem_minmax(0,1fr)_3.5rem] items-center gap-3 text-xs">
            <span className="text-slate-400">mrr</span>
            <QualityBar value={mrr} tone={mrr >= 0.55 ? "good" : "warn"} />
            <span className="text-right text-slate-300">{mrr ? mrr.toFixed(2) : "-"}</span>
          </div>
          <div className="grid grid-cols-[5rem_minmax(0,1fr)_3.5rem] items-center gap-3 text-xs">
            <span className="text-slate-400">graph</span>
            <QualityBar value={Math.min(1, graphLift * 4)} tone={graphLift > 0 ? "neutral" : "good"} />
            <span className="text-right text-slate-300">{percentText(totals.graphLift)}</span>
          </div>
        </div>

        <div className="rounded border border-slate-700/70 p-3 text-xs text-slate-300">
          <div className="font-semibold text-slate-100 mb-2">Tuning</div>
          <div className="flex justify-between gap-3">
            <span className="text-slate-500">Graph depth</span>
            <span>{graphExpansion?.enabled ? `${graphExpansion.depth ?? 0} / ${graphExpansion.neighborLimit ?? 0}` : "off"}</span>
          </div>
          <div className="mt-2 text-slate-500">Sources</div>
          <div className="mt-1 flex flex-wrap gap-1">
            {sourceOrder.slice(0, 5).map((source) => (
              <span key={source} className="rounded border border-slate-700 px-1.5 py-0.5 text-slate-300">
                {source}
              </span>
            ))}
            {!sourceOrder.length ? <span>-</span> : null}
          </div>
        </div>
      </div>

      {recommendations.length ? (
        <div className="rounded border border-slate-700/70 p-3 text-xs text-slate-300">
          <div className="font-semibold text-slate-100 mb-1">Recommended next action</div>
          <ul className="space-y-1">
            {recommendations.map((item) => (
              <li key={item}>{item}</li>
            ))}
          </ul>
        </div>
      ) : null}
    </section>
  );
}
