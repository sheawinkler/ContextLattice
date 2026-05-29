"use client";

type CountRow = {
  name?: string;
  count?: number;
};

type ProjectRow = {
  project?: string;
  docs?: number;
  connected_docs?: number;
  isolated_docs?: number;
  edges?: number;
  inferred_edges?: number;
  stale_inferred_edges?: number;
  explicit_edges?: number;
  density_edges_per_doc?: number;
  isolation_ratio?: number;
  max_node_degree?: number;
  overconnected_anchor_count?: number;
  quality_score?: number;
  quality_status?: string;
  quality_reasons?: string[];
  needs_backfill?: boolean;
  top_relations?: CountRow[];
};

type NodeRow = {
  memory_id?: string;
  degree?: number;
  inbound?: number;
  outbound?: number;
};

export type MemoryGraphPayload = {
  ok?: boolean;
  status?: string;
  generated_at?: string;
  doc_count?: number;
  edge_count?: number;
  connected_doc_count?: number;
  isolated_doc_count?: number;
  inferred_edge_count?: number;
  explicit_edge_count?: number;
  stale_inferred_edge_count?: number;
  density_edges_per_doc?: number;
  quality_status?: string;
  quality_score?: number;
  repair_project_count?: number;
  projects?: ProjectRow[];
  relations?: CountRow[];
  top_nodes?: NodeRow[];
  recommendations?: string[];
  edge_store?: {
    bytes?: number;
    path?: string;
  };
};

function numberValue(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function MiniBar({ value, max }: { value: number; max: number }) {
  const pct = max > 0 ? Math.max(0, Math.min(100, (value / max) * 100)) : 0;
  return (
    <div className="h-2 w-full rounded bg-slate-800 overflow-hidden" aria-hidden="true">
      <div className="h-full bg-cyan-300" style={{ width: `${pct}%` }} />
    </div>
  );
}

function Metric({
  label,
  value,
  tone,
}: {
  label: string;
  value: number | string;
  tone?: "warn" | "good";
}) {
  const toneClass =
    tone === "warn" ? "text-amber-200 border-amber-500/70" : tone === "good" ? "text-emerald-200 border-emerald-500/70" : "text-slate-200 border-slate-600";
  return (
    <div className={`rounded border px-3 py-2 ${toneClass}`}>
      <div className="text-xs uppercase tracking-wide text-slate-400">{label}</div>
      <div className="text-lg font-semibold">{typeof value === "number" ? value.toLocaleString() : value}</div>
    </div>
  );
}

function statusTone(status: string) {
  if (status === "repair_recommended" || status === "sparse" || status === "no_edges") {
    return "bg-amber-500 text-amber-950";
  }
  if (status === "watch") {
    return "bg-cyan-500 text-cyan-950";
  }
  return "bg-emerald-500 text-emerald-950";
}

export function MemoryGraphPanel({ graph }: { graph: MemoryGraphPayload | null }) {
  if (!graph) {
    return (
      <section className="card">
        <h3 className="text-lg font-semibold">Memory graph</h3>
        <p className="text-sm text-slate-400 mt-2">Graph telemetry unavailable.</p>
      </section>
    );
  }

  const projects = Array.isArray(graph.projects) ? graph.projects.slice(0, 6) : [];
  const relations = Array.isArray(graph.relations) ? graph.relations.slice(0, 8) : [];
  const topNodes = Array.isArray(graph.top_nodes) ? graph.top_nodes.slice(0, 6) : [];
  const maxProjectEdges = Math.max(1, ...projects.map((item) => numberValue(item.edges)));
  const maxRelationCount = Math.max(1, ...relations.map((item) => numberValue(item.count)));
  const status = String(graph.status || "unknown");
  const qualityStatus = String(graph.quality_status || status);

  return (
    <section className="card space-y-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 className="text-lg font-semibold">Memory graph</h3>
          <p className="text-xs text-slate-500 mt-1">
            {graph.generated_at ? `Updated ${new Date(graph.generated_at).toLocaleTimeString()}` : "Updated -"}
          </p>
        </div>
        <span
          className={`text-xs px-2 py-1 rounded ${statusTone(qualityStatus)}`}
        >
          {qualityStatus}
        </span>
      </div>

      <div className="grid md:grid-cols-6 gap-3 text-sm">
        <Metric label="Docs" value={numberValue(graph.doc_count)} />
        <Metric label="Edges" value={numberValue(graph.edge_count)} tone={numberValue(graph.edge_count) > 0 ? "good" : "warn"} />
        <Metric label="Inferred" value={numberValue(graph.inferred_edge_count)} />
        <Metric label="Isolated" value={numberValue(graph.isolated_doc_count)} tone={numberValue(graph.isolated_doc_count) > 0 ? "warn" : "good"} />
        <Metric label="Density" value={numberValue(graph.density_edges_per_doc).toFixed(2)} />
        <Metric label="Quality" value={numberValue(graph.quality_score) || "-"} tone={qualityStatus === "repair_recommended" ? "warn" : "good"} />
      </div>

      <div className="grid lg:grid-cols-2 gap-5">
        <div className="space-y-2">
          <h4 className="text-sm font-semibold text-slate-200">Projects</h4>
          {projects.length ? (
            <div className="space-y-2">
              {projects.map((item) => {
                const edges = numberValue(item.edges);
                return (
                  <div key={item.project || "project"} className="grid grid-cols-[minmax(0,1fr)_4rem] items-center gap-3 text-xs">
                    <div className="min-w-0">
                      <div className="flex items-center justify-between gap-2">
                        <span className="truncate text-slate-300">{item.project || "unknown"}</span>
                        <span className={`shrink-0 rounded px-1.5 py-0.5 ${statusTone(String(item.quality_status || "healthy"))}`}>
                          {numberValue(item.quality_score) || 0}
                        </span>
                      </div>
                      <MiniBar value={edges} max={maxProjectEdges} />
                      <div className="mt-1 flex flex-wrap gap-x-3 gap-y-1 text-slate-500">
                        <span>{numberValue(item.docs)} docs</span>
                        <span>{numberValue(item.isolated_docs)} isolated</span>
                        <span>{numberValue(item.overconnected_anchor_count)} anchors</span>
                      </div>
                    </div>
                    <div className="text-right text-slate-300">{edges.toLocaleString()}</div>
                  </div>
                );
              })}
            </div>
          ) : (
            <p className="text-xs text-slate-500">No project graph rows.</p>
          )}
        </div>

        <div className="space-y-2">
          <h4 className="text-sm font-semibold text-slate-200">Relations</h4>
          {relations.length ? (
            <div className="space-y-2">
              {relations.map((item) => {
                const count = numberValue(item.count);
                return (
                  <div key={item.name || "relation"} className="grid grid-cols-[minmax(0,1fr)_4rem] items-center gap-3 text-xs">
                    <div className="min-w-0">
                      <span className="truncate block text-slate-300">{item.name || "unknown"}</span>
                      <MiniBar value={count} max={maxRelationCount} />
                    </div>
                    <div className="text-right text-slate-300">{count.toLocaleString()}</div>
                  </div>
                );
              })}
            </div>
          ) : (
            <p className="text-xs text-slate-500">No relation rows.</p>
          )}
        </div>
      </div>

      {topNodes.length ? (
        <div className="overflow-x-auto text-xs">
          <h4 className="text-sm font-semibold text-slate-200 mb-2">Top connected memories</h4>
          <table className="w-full border border-slate-700/60 rounded">
            <thead className="bg-slate-900/40 text-slate-300">
              <tr>
                <th className="px-3 py-2 text-left">Memory</th>
                <th className="px-3 py-2 text-right">Degree</th>
                <th className="px-3 py-2 text-right">In</th>
                <th className="px-3 py-2 text-right">Out</th>
              </tr>
            </thead>
            <tbody>
              {topNodes.map((node) => (
                <tr key={node.memory_id || "node"} className="border-t border-slate-800">
                  <td className="px-3 py-2 max-w-[32rem] truncate">{node.memory_id || "unknown"}</td>
                  <td className="px-3 py-2 text-right">{numberValue(node.degree)}</td>
                  <td className="px-3 py-2 text-right">{numberValue(node.inbound)}</td>
                  <td className="px-3 py-2 text-right">{numberValue(node.outbound)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {Array.isArray(graph.recommendations) && graph.recommendations.length ? (
        <div className="rounded border border-slate-700/70 p-3 text-xs text-slate-300">
          <div className="font-semibold text-slate-100 mb-1">Recommended next action</div>
          <ul className="space-y-1">
            {graph.recommendations.slice(0, 3).map((item) => (
              <li key={item}>{item}</li>
            ))}
          </ul>
        </div>
      ) : null}
    </section>
  );
}
