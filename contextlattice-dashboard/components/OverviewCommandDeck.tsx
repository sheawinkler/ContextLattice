"use client";

import { useEffect, useMemo, useState } from "react";
import {
  asArray,
  asRecord,
  estimateTokenImpact,
  formatCompact,
  formatTimestamp,
  serviceHealthLabel,
  toInt,
  toText,
} from "@/lib/dashboardMetrics";

async function getJson(path: string): Promise<any | null> {
  try {
    const response = await fetch(path, { cache: "no-store" });
    return await response.json();
  } catch {
    return null;
  }
}

function CapabilityCard({
  eyebrow,
  title,
  body,
  metric,
}: {
  eyebrow: string;
  title: string;
  body: string;
  metric: string;
}) {
  return (
    <article className="cl-capability-card">
      <div className="cl-capability-top">
        <span className="cl-kicker">{eyebrow}</span>
        <strong>{metric}</strong>
      </div>
      <h3>{title}</h3>
      <p>{body}</p>
    </article>
  );
}

export function OverviewCommandDeck() {
  const [overview, setOverview] = useState<any | null>(null);
  const [mindmap, setMindmap] = useState<any | null>(null);
  const [topics, setTopics] = useState<any | null>(null);

  useEffect(() => {
    let mounted = true;
    Promise.all([
      getJson("/api/telemetry/overview?recallHistory=24&toolLimit=12"),
      getJson("/api/telemetry/mindmap?project=__all__&depth=4&limit=700"),
      getJson("/api/memory/topics?depth=4"),
    ]).then(([overviewData, mindmapData, topicsData]) => {
      if (!mounted) return;
      setOverview(overviewData);
      setMindmap(mindmapData);
      setTopics(topicsData);
    });
    return () => {
      mounted = false;
    };
  }, []);

  const tokenImpact = useMemo(() => estimateTokenImpact(overview, mindmap, topics), [overview, mindmap, topics]);
  const health = serviceHealthLabel(overview);
  const summary = asRecord(asRecord(mindmap).summary);
  const projects = asArray(asRecord(mindmap).projects);
  const activeAgents = toInt(asRecord(asRecord(overview).agentRuntime).active);
  const retrievalAlerts = asArray(asRecord(asRecord(asRecord(overview).retrieval).alerts).active).length;
  const metadataContract = asRecord(asRecord(asRecord(overview).status).metadataContract);
  const status = asRecord(asRecord(overview).status);
  const runtimePolicy = asRecord(status.runtimeBackendPolicy);
  const generatedAt = toText(asRecord(mindmap).generatedAt) || toText(asRecord(overview).capturedAt);
  const vectorBackend = toText(runtimePolicy.vector_backend).replace(/_/g, " ");

  return (
    <div className="cl-page cl-overview-page">
      <section className="cl-hero cl-hero--compact">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Overview // intelligence layer</p>
          <h2>The work survives the window.</h2>
          <p>
            ContextLattice is the local-first intelligence layer that gives AI agents durable continuity,
            explainable retrieval, portable context, and verified learning across harnesses.
          </p>
        </div>
        <div className="cl-overview-stamp">
          <span>snapshot</span>
          <strong>{formatTimestamp(generatedAt)}</strong>
        </div>
      </section>

      <section className="cl-proof-strip">
        <div>
          <span className="cl-label">health</span>
          <strong>{health.label}</strong>
        </div>
        <div>
          <span className="cl-label">projects</span>
          <strong>{formatCompact(projects.length)}</strong>
        </div>
        <div>
          <span className="cl-label">topics</span>
          <strong>{formatCompact(toInt(summary.totalNodes))}</strong>
        </div>
        <div>
          <span className="cl-label">events</span>
          <strong>{formatCompact(toInt(summary.totalEvents))}</strong>
        </div>
        <div>
          <span className="cl-label">estimated tokens saved</span>
          <strong>{formatCompact(tokenImpact.estimatedSaved)}</strong>
        </div>
      </section>

      <section className="cl-capability-grid">
        <CapabilityCard
          eyebrow="reopen"
          title="Durable continuity"
          body="The mission survives chats, agents, accounts, and restarts through replayable writes, checkpoints, objective lineage, sessions, and proof-linked handoffs."
          metric={`${formatCompact(toInt(metadataContract.totalWrites))} writes`}
        />
        <CapabilityCard
          eyebrow="select"
          title="Explainable retrieval"
          body="Retrieval lanes, rollups, graph evidence, and agent packs compete for limited prompt space so every selected token can justify its place."
          metric={`${tokenImpact.confidence} confidence`}
        />
        <CapabilityCard
          eyebrow="move"
          title="Portable context"
          body="Signed packets, passports, encrypted continuations, and stable agent contracts carry bounded work across harnesses without surrendering lineage or execution control."
          metric={`${formatCompact(activeAgents)} agents`}
        />
        <CapabilityCard
          eyebrow="earn"
          title="Verified skill evolution"
          body="Outcomes, retrieval receipts, holdouts, and human approval stand between a repeated pattern and an activated skill or policy."
          metric={`${formatCompact(toInt(summary.coveredTopics))} covered`}
        />
        <CapabilityCard
          eyebrow="compound"
          title="Aggregate Signal"
          body="Explicitly opted-in, clipped statistics can queue locally while raw prompts and memory stay put; production learning remains proof-held."
          metric="preview"
        />
        <CapabilityCard
          eyebrow="prove"
          title="Runtime truth, not theater"
          body="Go/Rust hot paths carry the live stack while health, retrieval alerts, token impact, project density, and exclusions stay visible here."
          metric={vectorBackend || `${formatCompact(retrievalAlerts)} alerts`}
        />
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">project density</p>
            <h3>Where memory is concentrating</h3>
          </div>
          <a className="cl-text-link" href="/mindmap">Inspect topic hierarchy</a>
        </div>
        <div className="cl-project-grid">
          {projects.slice(0, 8).map((project) => {
            const row = asRecord(project);
            return (
              <article key={toText(row.project)} className="cl-project-card">
                <span>{toText(row.project) || "workspace"}</span>
                <strong>{formatCompact(toInt(row.events))}</strong>
                <small>{formatCompact(toInt(row.topics))} topics · {formatCompact(toInt(row.uniqueSessions))} sessions</small>
              </article>
            );
          })}
          {!projects.length ? <p className="cl-empty">Project rollups are not loaded yet.</p> : null}
        </div>
      </section>
    </div>
  );
}
