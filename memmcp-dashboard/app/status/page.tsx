"use client";

import { useEffect, useState } from "react";

type Service = {
  name: string;
  healthy: boolean;
  detail?: string;
};

type StatusPayload = {
  services?: Service[];
};

type PreferenceSummary = {
  total?: number;
  positive?: string[];
  negative?: string[];
  notes?: string[];
  updated_at?: string;
};

type PreferencesPayload = {
  enabled?: boolean;
  preferences?: PreferenceSummary;
};

type TopicNode = {
  count?: number;
  children?: Record<string, TopicNode>;
};

type TopicsPayload = {
  topics?: Record<string, TopicNode>;
  project?: string;
};

type MemoryTelemetry = {
  updatedAt?: string;
  lastWriteAt?: string | null;
  lastWriteLatencyMs?: number | null;
  memoryBank?: {
    queueDepth?: number;
    queueMax?: number;
    workers?: number;
    processed?: number;
    dropped?: number;
  };
  fanout?: {
    queueDepth?: number;
    queueMax?: number;
    workers?: number;
    processed?: number;
    dropped?: number;
  };
};

export default function StatusPage() {
  const [status, setStatus] = useState<StatusPayload | null>(null);
  const [preferences, setPreferences] = useState<PreferencesPayload | null>(null);
  const [topics, setTopics] = useState<TopicsPayload | null>(null);
  const [memoryTelemetry, setMemoryTelemetry] = useState<MemoryTelemetry | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [updatedAt, setUpdatedAt] = useState<string | null>(null);

  async function loadStatus() {
    try {
      setError(null);
      const [statusRes, prefRes, topicRes, memRes] = await Promise.all([
        fetch("/api/memory/status", { cache: "no-store" }),
        fetch("/api/memory/preferences", { cache: "no-store" }),
        fetch("/api/memory/topics", { cache: "no-store" }),
        fetch("/api/telemetry/memory", { cache: "no-store" }),
      ]);
      const statusData = await statusRes.json();
      if (!statusRes.ok) {
        throw new Error(statusData?.error || "Status request failed");
      }
      setStatus(statusData);
      if (prefRes.ok) {
        setPreferences(await prefRes.json());
      }
      if (topicRes.ok) {
        setTopics(await topicRes.json());
      }
      if (memRes.ok) {
        setMemoryTelemetry(await memRes.json());
      }
      setUpdatedAt(new Date().toLocaleTimeString());
    } catch (err: any) {
      setError(err?.message || "Status unavailable");
    }
  }

  useEffect(() => {
    loadStatus();
    const handle = setInterval(loadStatus, 15000);
    return () => clearInterval(handle);
  }, []);

  return (
    <div className="space-y-6">
      <section className="card">
        <h2 className="text-xl font-semibold">Stack status</h2>
        <p className="text-sm text-slate-400 mt-1">
          Live health view of the orchestrator and its upstream dependencies.
        </p>
        <div className="text-xs text-slate-500 mt-2">
          Last updated: {updatedAt || "—"}
        </div>
        <div className="mt-4 flex flex-wrap gap-2">
          <button
            className="rounded border border-slate-700 px-3 py-2 text-sm"
            onClick={loadStatus}
          >
            Refresh now
          </button>
          <a
            className="rounded border border-slate-700 px-3 py-2 text-sm"
            href="http://127.0.0.1:8075/status/ui"
            target="_blank"
            rel="noreferrer"
          >
            Open status UI
          </a>
        </div>
      </section>

      {error ? (
        <section className="card">
          <h3 className="text-lg font-semibold">Status error</h3>
          <p className="text-sm text-amber-300 mt-2">{error}</p>
        </section>
      ) : null}

      <section className="grid md:grid-cols-2 gap-4">
        {(status?.services || []).map((service) => (
          <div key={service.name} className="card space-y-2">
            <div className="flex items-center justify-between">
              <h3 className="text-lg font-semibold capitalize">{service.name}</h3>
              <span
                className={`text-xs px-2 py-1 rounded ${
                  service.healthy
                    ? "bg-emerald-500 text-emerald-950"
                    : "bg-rose-500 text-rose-950"
                }`}
              >
                {service.healthy ? "healthy" : "down"}
              </span>
            </div>
            <p className="text-xs text-slate-400">{service.detail || "—"}</p>
          </div>
        ))}
        {!status?.services?.length ? (
          <div className="card">
            <p className="text-sm text-slate-400">Status data not available yet.</p>
          </div>
        ) : null}
      </section>

      <section className="grid md:grid-cols-2 gap-4">
        <div className="card space-y-3">
          <div className="flex items-center justify-between">
            <h3 className="text-lg font-semibold">Learning loop</h3>
            <span
              className={`text-xs px-2 py-1 rounded ${
                preferences?.enabled ? "bg-emerald-500 text-emerald-950" : "bg-slate-500 text-slate-950"
              }`}
            >
              {preferences?.enabled ? "enabled" : "disabled"}
            </span>
          </div>
          <div className="text-sm text-slate-300">
            Preference entries: {preferences?.preferences?.total ?? 0}
          </div>
          <div className="text-xs text-slate-500">
            Last updated: {preferences?.preferences?.updated_at || "—"}
          </div>
        </div>
        <div className="card space-y-3">
          <h3 className="text-lg font-semibold">Topic retrieval tree</h3>
          <div className="text-sm text-slate-300">
            Projects indexed: {topics?.topics ? Object.keys(topics.topics).length : 0}
          </div>
          <div className="text-xs text-slate-500">
            Top-level:{" "}
            {topics?.topics
              ? Object.keys(topics.topics)
                  .slice(0, 3)
                  .join(", ") || "—"
              : "—"}
          </div>
        </div>
        <div className="card space-y-3">
          <h3 className="text-lg font-semibold">Memory write throughput</h3>
          <div className="text-sm text-slate-300">
            Queue depth: {memoryTelemetry?.memoryBank?.queueDepth ?? 0} / {memoryTelemetry?.memoryBank?.queueMax ?? 0}
          </div>
          <div className="text-sm text-slate-300">
            Fanout depth: {memoryTelemetry?.fanout?.queueDepth ?? 0} / {memoryTelemetry?.fanout?.queueMax ?? 0}
          </div>
          <div className="text-xs text-slate-500">
            Last write latency: {memoryTelemetry?.lastWriteLatencyMs ?? "—"} ms
          </div>
        </div>
      </section>
    </div>
  );
}
