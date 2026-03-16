"use client";

import { FormEvent, useEffect, useMemo, useRef, useState } from "react";

type RetrievalRow = {
  project?: string;
  file?: string;
  summary?: string;
  score?: number;
  source?: string;
  sources?: string[];
  topic_path?: string;
  memory_id?: string;
};

type SourceSummary = {
  sources?: string[];
  returned_now?: string[];
  pending_sources?: string[];
  warming_sources?: string[];
  timed_out_sources?: string[];
  failed_sources?: string[];
  budget_exceeded_sources?: string[];
  skipped_sources?: string[];
};

type RetrievalLifecycle = {
  status?: string;
  partial?: boolean;
  result_state?: string;
  next_actions?: string[];
};

type ContinuationInfo = {
  token?: string;
  events_url?: string;
  pending_sources?: string[];
};

type SearchResponse = {
  results?: RetrievalRow[];
  warnings?: string[];
  source_summary?: SourceSummary;
  retrieval_lifecycle?: RetrievalLifecycle;
  continuation_async?: ContinuationInfo;
};

type ContinuationEvent = {
  event: string;
  payload: string;
  at: string;
};

function normalizeSourceList(values: string[] | undefined): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const value of values || []) {
    const normalized = String(value || "").trim();
    if (!normalized || seen.has(normalized)) {
      continue;
    }
    seen.add(normalized);
    out.push(normalized);
  }
  return out;
}

function rowSources(row: RetrievalRow): string[] {
  const combined = normalizeSourceList([
    ...(Array.isArray(row.sources) ? row.sources : []),
    String(row.source || "").trim(),
  ]);
  return combined;
}

function rowMemoryId(row: RetrievalRow): string {
  const explicit = String(row.memory_id || "").trim();
  if (explicit) {
    return explicit;
  }
  const project = String(row.project || "").trim();
  const file = String(row.file || "").trim();
  if (project && file) {
    return `${project}::${file}`;
  }
  return "";
}

function SourceChip({ label, value }: { label: string; value: string[] | undefined }) {
  const items = normalizeSourceList(value);
  return (
    <div className="rounded border border-slate-700 px-2 py-1">
      <div className="text-[10px] uppercase tracking-wide text-slate-500">{label}</div>
      <div className="text-xs text-slate-200 mt-1">{items.length ? items.join(", ") : "--"}</div>
    </div>
  );
}

export function RetrievalPanel() {
  const [project, setProject] = useState("contextlattice");
  const [query, setQuery] = useState("");
  const [topicPath, setTopicPath] = useState("");
  const [mode, setMode] = useState("balanced");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [response, setResponse] = useState<SearchResponse | null>(null);
  const [events, setEvents] = useState<ContinuationEvent[]>([]);
  const [rawById, setRawById] = useState<Record<string, string>>({});
  const [loadingRaw, setLoadingRaw] = useState<Record<string, boolean>>({});
  const eventSourceRef = useRef<EventSource | null>(null);

  useEffect(() => {
    return () => {
      eventSourceRef.current?.close();
      eventSourceRef.current = null;
    };
  }, []);

  const sortedResults = useMemo(() => {
    const rows = [...(response?.results || [])];
    rows.sort((a, b) => {
      const aRollup = rowSources(a).includes("topic_rollups") ? 1 : 0;
      const bRollup = rowSources(b).includes("topic_rollups") ? 1 : 0;
      if (aRollup !== bRollup) {
        return bRollup - aRollup;
      }
      const aScore = typeof a.score === "number" ? a.score : 0;
      const bScore = typeof b.score === "number" ? b.score : 0;
      return bScore - aScore;
    });
    return rows;
  }, [response?.results]);

  function appendEvent(event: string, payload: string) {
    setEvents((prev) => [{ event, payload, at: new Date().toLocaleTimeString() }, ...prev].slice(0, 30));
  }

  function closeEventStream() {
    eventSourceRef.current?.close();
    eventSourceRef.current = null;
  }

  function openContinuationStream(token: string) {
    closeEventStream();
    const stream = new EventSource(`/api/memory/search/continuations/${encodeURIComponent(token)}/events`);
    eventSourceRef.current = stream;

    const eventNames = ["snapshot", "update", "ready", "heartbeat", "completed", "failed"];
    for (const name of eventNames) {
      stream.addEventListener(name, (evt) => {
        const message = evt as MessageEvent<string>;
        appendEvent(name, message.data || "");
      });
    }

    stream.onerror = () => {
      appendEvent("stream_error", "Continuation stream closed or unavailable");
      closeEventStream();
    };
  }

  async function runSearch(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (!query.trim()) {
      setError("Query is required");
      return;
    }
    setLoading(true);
    setError(null);
    setResponse(null);
    setEvents([]);
    setRawById({});
    closeEventStream();

    const payload: Record<string, unknown> = {
      project: project.trim(),
      query: query.trim(),
      retrieval_mode: mode,
      include_grounding: true,
      limit: 20,
      agent_id: "dashboard",
    };
    if (topicPath.trim()) {
      payload.topic_path = topicPath.trim();
    }

    try {
      const res = await fetch("/api/memory/search", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(payload),
      });
      const data = (await res.json()) as SearchResponse;
      if (!res.ok) {
        const detail = typeof data === "object" ? JSON.stringify(data) : "search failed";
        throw new Error(detail);
      }
      setResponse(data);

      const token = String(data.continuation_async?.token || "").trim();
      if (token) {
        openContinuationStream(token);
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : "Search failed";
      setError(message);
    } finally {
      setLoading(false);
    }
  }

  async function loadRaw(memoryId: string) {
    if (!memoryId) {
      return;
    }
    setLoadingRaw((prev) => ({ ...prev, [memoryId]: true }));
    try {
      const res = await fetch(`/api/memory/raw?memory_id=${encodeURIComponent(memoryId)}`, {
        cache: "no-store",
      });
      const data = (await res.json()) as { memory?: { content?: string } };
      const content = String(data?.memory?.content || "").trim();
      setRawById((prev) => ({ ...prev, [memoryId]: content || "(no raw content found)" }));
    } catch {
      setRawById((prev) => ({ ...prev, [memoryId]: "(failed to load raw content)" }));
    } finally {
      setLoadingRaw((prev) => ({ ...prev, [memoryId]: false }));
    }
  }

  const summary = response?.source_summary;
  const lifecycle = response?.retrieval_lifecycle;

  return (
    <section className="card space-y-4">
      <div>
        <h3 className="text-lg font-semibold">Retrieval flow</h3>
        <p className="text-sm text-slate-400 mt-1">
          Fast-now retrieval with deep continuation visibility and source-level status.
        </p>
      </div>

      <form className="grid gap-3 md:grid-cols-6" onSubmit={runSearch}>
        <label className="md:col-span-1 text-sm text-slate-300">
          Project
          <input
            className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-2 text-sm"
            value={project}
            onChange={(evt) => setProject(evt.target.value)}
          />
        </label>
        <label className="md:col-span-2 text-sm text-slate-300">
          Query
          <input
            className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-2 text-sm"
            value={query}
            onChange={(evt) => setQuery(evt.target.value)}
            placeholder="profitability tuning baseline ladder"
          />
        </label>
        <label className="md:col-span-2 text-sm text-slate-300">
          Topic path (optional)
          <input
            className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-2 text-sm"
            value={topicPath}
            onChange={(evt) => setTopicPath(evt.target.value)}
            placeholder="runbooks/performance"
          />
        </label>
        <label className="md:col-span-1 text-sm text-slate-300">
          Mode
          <select
            className="mt-1 w-full rounded border border-slate-700 bg-slate-950 px-2 py-2 text-sm"
            value={mode}
            onChange={(evt) => setMode(evt.target.value)}
          >
            <option value="fast">fast</option>
            <option value="balanced">balanced</option>
            <option value="deep">deep</option>
          </select>
        </label>
        <div className="md:col-span-6 flex gap-2">
          <button
            type="submit"
            disabled={loading}
            className="rounded border border-slate-700 px-3 py-2 text-sm disabled:opacity-60"
          >
            {loading ? "Searching..." : "Search"}
          </button>
          <button
            type="button"
            onClick={closeEventStream}
            className="rounded border border-slate-700 px-3 py-2 text-sm"
          >
            Stop continuation stream
          </button>
        </div>
      </form>

      {error ? <div className="text-sm text-rose-300">{error}</div> : null}

      <div className="grid gap-2 md:grid-cols-3">
        <SourceChip label="Returned now" value={summary?.returned_now} />
        <SourceChip label="Pending" value={summary?.pending_sources} />
        <SourceChip label="Warming" value={summary?.warming_sources} />
        <SourceChip label="Timed out" value={summary?.timed_out_sources} />
        <SourceChip label="Failed" value={summary?.failed_sources} />
        <SourceChip label="Skipped/Budget" value={[
          ...(summary?.skipped_sources || []),
          ...(summary?.budget_exceeded_sources || []),
        ]} />
      </div>

      {response?.warnings?.length ? (
        <div className="rounded border border-amber-700/40 bg-amber-950/30 p-3 text-xs text-amber-100">
          {response.warnings.map((warning) => (
            <div key={warning}>- {warning}</div>
          ))}
        </div>
      ) : null}

      <div className="text-xs text-slate-400">
        Lifecycle: {lifecycle?.status || "--"} / {lifecycle?.result_state || "--"}
        {lifecycle?.partial ? " (partial)" : ""}
      </div>

      <div className="space-y-3">
        {sortedResults.length === 0 ? (
          <div className="text-sm text-slate-400">No results yet.</div>
        ) : (
          sortedResults.map((row, idx) => {
            const memoryId = rowMemoryId(row);
            const rowKey = `${memoryId || "row"}-${idx}`;
            const sources = rowSources(row);
            return (
              <article key={rowKey} className="rounded border border-slate-800 p-3 space-y-2">
                <div className="flex flex-wrap gap-2 text-[11px] text-slate-400">
                  <span>score: {typeof row.score === "number" ? row.score.toFixed(3) : "--"}</span>
                  <span>project: {row.project || "--"}</span>
                  <span>file: {row.file || "--"}</span>
                  <span>topic: {row.topic_path || "--"}</span>
                </div>
                <div className="text-sm text-slate-100 whitespace-pre-wrap">{row.summary || "(no summary)"}</div>
                <div className="flex flex-wrap gap-2">
                  {sources.length ? (
                    sources.map((source) => (
                      <span
                        key={source}
                        className="rounded border border-cyan-800 bg-cyan-950/30 px-2 py-1 text-[11px] text-cyan-100"
                      >
                        {source}
                      </span>
                    ))
                  ) : (
                    <span className="text-xs text-slate-500">No source metadata</span>
                  )}
                </div>
                <div className="flex items-center gap-2">
                  <button
                    type="button"
                    onClick={() => loadRaw(memoryId)}
                    disabled={!memoryId || !!loadingRaw[memoryId]}
                    className="rounded border border-slate-700 px-2 py-1 text-xs disabled:opacity-50"
                  >
                    {loadingRaw[memoryId] ? "Loading raw..." : "Load raw evidence"}
                  </button>
                  <span className="text-[11px] text-slate-500">{memoryId || "no memory id"}</span>
                </div>
                {rawById[memoryId] ? (
                  <pre className="max-h-44 overflow-auto rounded bg-slate-950 p-2 text-[11px] text-slate-200 whitespace-pre-wrap">
                    {rawById[memoryId]}
                  </pre>
                ) : null}
              </article>
            );
          })
        )}
      </div>

      <div className="rounded border border-slate-800 p-3">
        <div className="text-xs uppercase tracking-wide text-slate-500">Continuation events</div>
        {events.length === 0 ? (
          <div className="text-sm text-slate-400 mt-2">No stream events yet.</div>
        ) : (
          <div className="mt-2 max-h-40 overflow-auto space-y-1 text-xs text-slate-300">
            {events.map((evt, idx) => (
              <div key={`${evt.at}-${evt.event}-${idx}`} className="rounded border border-slate-800 p-2">
                <div className="text-slate-500">[{evt.at}] {evt.event}</div>
                <div className="mt-1 whitespace-pre-wrap">{evt.payload}</div>
              </div>
            ))}
          </div>
        )}
      </div>
    </section>
  );
}
