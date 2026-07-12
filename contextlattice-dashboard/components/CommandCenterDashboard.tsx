"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  ageLabel,
  asArray,
  asRecord,
  estimateContextPackQuality,
  estimateRunnerQuality,
  estimateTokenImpact,
  formatCompact,
  formatMs,
  formatTimestamp,
  serviceHealthLabel,
  toInt,
  toNumber,
  toText,
} from "@/lib/dashboardMetrics";
import { AgentPacketWorkbench } from "@/components/AgentPacketWorkbench";

type JsonResult = {
  data: any | null;
  error: string | null;
};

type DashboardState = {
  overview: any | null;
  mindmap: any | null;
  topics: any | null;
  health: any | null;
};

const EMPTY_STATE: DashboardState = {
  overview: null,
  mindmap: null,
  topics: null,
  health: null,
};

async function loadJson(path: string): Promise<JsonResult> {
  try {
    const response = await fetch(path, { cache: "no-store" });
    const data = await response.json().catch(() => null);
    if (!response.ok) {
      return { data, error: `${path} -> ${response.status}` };
    }
    return { data, error: null };
  } catch (error) {
    return { data: null, error: `${path} -> ${error instanceof Error ? error.message : String(error)}` };
  }
}

function MetricCard({
  label,
  value,
  detail,
  tone = "neutral",
  compactValue = false,
}: {
  label: string;
  value: string;
  detail: string;
  tone?: "neutral" | "good" | "warn" | "hot";
  compactValue?: boolean;
}) {
  return (
    <article className={`cl-metric cl-metric--${tone}`}>
      <div className="cl-label">{label}</div>
      <div className={`cl-metric-value${compactValue ? " cl-metric-value--compact" : ""}`}>{value}</div>
      <div className="cl-metric-detail">{detail}</div>
    </article>
  );
}

function LaneBar({ name, metrics }: { name: string; metrics: any }) {
  const p95 = toNumber(metrics?.p95Ms);
  const requests = toInt(metrics?.requests);
  const errors = toInt(metrics?.errors);
  const timeouts = toInt(metrics?.timeouts);
  const width = Math.max(4, Math.min(100, (p95 / 26000) * 100));
  const tone = errors || timeouts || p95 > 7000 ? "warn" : "good";
  return (
    <div className="cl-lane-row">
      <div className="cl-lane-head">
        <span>{name}</span>
        <span>{formatMs(p95)} p95</span>
      </div>
      <div className="cl-lane-track" aria-hidden="true">
        <div className={`cl-lane-fill cl-lane-fill--${tone}`} style={{ width: `${width}%` }} />
      </div>
      <div className="cl-lane-foot">
        <span>{formatCompact(requests)} req</span>
        <span>{formatCompact(errors)} err</span>
        <span>{formatCompact(timeouts)} timeout</span>
      </div>
    </div>
  );
}

function SessionRow({ session }: { session: any }) {
  const rollup = asRecord(session?.rollup);
  const contribution = asRecord(session?.memory_contribution);
  const objective = toText(session?.objective) || toText(rollup.objective) || "active agent session";
  const nextAction = toText(session?.next_action) || toText(rollup.next_action) || "--";
  return (
    <div className="cl-session-row">
      <div>
        <div className="cl-session-title">{toText(session?.agent_id) || toText(session?.agent) || "agent"}</div>
        <div className="cl-session-subtitle">{objective.slice(0, 118)}</div>
      </div>
      <div className="cl-session-meta">
        <span>{formatCompact(toInt(contribution.score))} memory</span>
        <span>{ageLabel(session?.last_event_at)}</span>
      </div>
      <div className="cl-session-next">{nextAction.slice(0, 150)}</div>
    </div>
  );
}

function formatPercent(value: number): string {
  return `${Math.round(value * 100)}%`;
}

function formatSigned(value: number): string {
  if (!value) return "0";
  return `${value > 0 ? "+" : "-"}${formatCompact(Math.abs(value))}`;
}

export function CommandCenterDashboard() {
  const [state, setState] = useState<DashboardState>(EMPTY_STATE);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [errors, setErrors] = useState<string[]>([]);
  const [updatedAt, setUpdatedAt] = useState<string>("");

  const load = useCallback(async (initial = false) => {
    if (initial) setLoading(true);
    else setRefreshing(true);
    const [overview, mindmap, topics, health] = await Promise.all([
      loadJson("/api/telemetry/overview?recallHistory=48&toolLimit=24"),
      loadJson("/api/telemetry/mindmap?project=__all__&depth=4&limit=700"),
      loadJson("/api/memory/topics?depth=4"),
      loadJson("/api/health"),
    ]);
    setState({
      overview: overview.data,
      mindmap: mindmap.data,
      topics: topics.data,
      health: health.data,
    });
    setErrors([overview.error, mindmap.error, topics.error, health.error].filter(Boolean) as string[]);
    setUpdatedAt(new Date().toLocaleTimeString());
    setLoading(false);
    setRefreshing(false);
  }, []);

  useEffect(() => {
    void load(true);
    const handle = window.setInterval(() => void load(false), 15000);
    return () => window.clearInterval(handle);
  }, [load]);

  const overview = state.overview;
  const mindmap = state.mindmap;
  const health = state.health;
  const serviceHealth = serviceHealthLabel(overview);
  const tokenImpact = useMemo(
    () => estimateTokenImpact(overview, mindmap, state.topics),
    [overview, mindmap, state.topics],
  );
  const contextPackQuality = useMemo(() => estimateContextPackQuality(overview), [overview]);
  const runnerQuality = useMemo(() => estimateRunnerQuality(overview), [overview]);
  const tokenTruth = asRecord(asRecord(overview).tokenImpact);
  const tokenSampleCount = toInt(tokenTruth.sample_count);
  const transportSampleCount = toInt(tokenTruth.transport_inclusive_sample_count);
  const transportTokensExact = toNumber(tokenTruth.transport_tokens_exact);
  const compiledPromptTokens = toNumber(tokenTruth.compiled_prompt_tokens_estimate);
  const netTokenDelta = toNumber(tokenTruth.net_token_delta);
  const transportComplete = tokenSampleCount > 0 && transportSampleCount === tokenSampleCount;
  const agentRuntime = asRecord(asRecord(overview).agentRuntime);
  const liveSessionCount = toInt(agentRuntime.live ?? agentRuntime.active);
  const expiredSessionCount = toInt(agentRuntime.expired);
  const totalSessionCount = toInt(agentRuntime.total);
  const memoryTelemetry = asRecord(asRecord(overview).memoryTelemetry);
  const memoryBank = asRecord(memoryTelemetry.memoryBank);
  const fanout = asRecord(memoryTelemetry.fanout);
  const retrieval = asRecord(asRecord(overview).retrieval);
  const retrievalAlerts = asArray(asRecord(retrieval.alerts).active);
  const sourceRows = Object.entries(asRecord(asRecord(retrieval.latency).sources))
    .map(([source, metrics]) => ({ source, metrics }))
    .sort((a, b) => toNumber(asRecord(b.metrics).p95Ms) - toNumber(asRecord(a.metrics).p95Ms))
    .slice(0, 7);
  const sessions = asArray(agentRuntime.sessions).slice(0, 5);
  const topPaths = asArray(asRecord(mindmap).topPaths).slice(0, 8);
  const statusWarnings = asArray(asRecord(asRecord(overview).status).warnings).map(String);
  const endpointErrors = Object.values(asRecord(asRecord(overview).errors)).map(String);
  const alertLines = retrievalAlerts.map((alert) => {
    const row = asRecord(alert);
    return `${toText(row.severity) || "signal"}: ${toText(row.source) || "retrieval"} ${toText(row.message)}`.trim();
  });
  const signals = [...alertLines, ...statusWarnings, ...endpointErrors].filter(Boolean).slice(0, 8);
  const healthStatus = toText(health?.status) || (health?.ok ? "healthy" : "unknown");
  const storage = asRecord(asRecord(asRecord(overview).status).storageGovernance).disk;
  const storageRecord = asRecord(storage);
  const diskPressure = toText(storageRecord.pressureBand) || toText(asRecord(asRecord(overview).storage).pressureBand) || "tracked";

  return (
    <div className="cl-page cl-console-page">
      <section className="cl-hero">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Lattice // live operations</p>
          <h2>Memory that earns its tokens.</h2>
          <p>
            A hard proof cockpit for the local memory stack: what is healthy, what is expensive,
            what is being recalled, and how much context the lattice kept out of the prompt.
          </p>
        </div>
        <div className="cl-hero-panel" aria-label="Live runtime summary">
          <span className={`cl-status-dot cl-status-dot--${healthStatus === "healthy" ? "good" : "warn"}`} />
          <div>
            <div className="cl-label">orchestrator</div>
            <div className="cl-hero-stat">{healthStatus}</div>
            <div className="cl-metric-detail">updated {updatedAt || "--"}</div>
          </div>
          <button className="cl-button" type="button" onClick={() => void load(false)} disabled={refreshing}>
            {refreshing ? "Refreshing" : "Refresh"}
          </button>
        </div>
      </section>

      {errors.length ? (
        <section className="cl-alert-strip">
          <strong>Dashboard fetch degraded.</strong>
          <span>{errors.join(" | ")}</span>
        </section>
      ) : null}

      <section className="cl-metric-grid" aria-busy={loading}>
        <MetricCard
          label="token truth"
          value={transportSampleCount ? formatCompact(transportTokensExact) : "--"}
          detail={`${transportSampleCount}/${tokenSampleCount} wire-measured · net ${formatSigned(netTokenDelta)} · compiled ${formatCompact(compiledPromptTokens)}`}
          tone={transportSampleCount && netTokenDelta < 0 ? "warn" : transportComplete ? "good" : "neutral"}
          compactValue={!transportSampleCount}
        />
        <MetricCard
          label={tokenImpact.calibrationGrade === "heuristic" ? "estimated context savings" : "measured context savings"}
          value={formatCompact(tokenImpact.estimatedSaved)}
          detail={`${tokenImpact.compressionRatio}x compression · ${tokenImpact.confidence} confidence`}
          tone="hot"
        />
        <MetricCard
          label="session truth"
          value={formatCompact(liveSessionCount)}
          detail={`${formatCompact(totalSessionCount)} total · ${formatCompact(expiredSessionCount)} expired · ${formatCompact(agentRuntime.idle_ttl_seconds)}s idle TTL`}
          tone={liveSessionCount > 0 ? "good" : "neutral"}
        />
        <MetricCard
          label="learning loop"
          value={formatCompact(contextPackQuality.outcomeSamples)}
          detail={`${contextPackQuality.observedFirstPassRate === null ? "--" : formatPercent(contextPackQuality.observedFirstPassRate)} first pass · ${contextPackQuality.observedRepairRate === null ? "--" : formatPercent(contextPackQuality.observedRepairRate)} repair`}
          tone={contextPackQuality.outcomeSamples > 0 ? "good" : "neutral"}
        />
        <MetricCard
          label="modeled inference avoided"
          value={formatCompact(contextPackQuality.modeledInferenceSaved)}
          detail={`${contextPackQuality.extraCallsAvoided} calls · ${formatCompact(contextPackQuality.calibrationOutcomeSamples)} calibrated · ${contextPackQuality.confidence}`}
          tone={contextPackQuality.calibrationGrade === "outcome_adjusted" ? "good" : "neutral"}
        />
        <MetricCard
          label="observed provider tokens"
          value={formatCompact(contextPackQuality.observedProviderTotalTokens)}
          detail={`${formatCompact(contextPackQuality.observedProviderUsageCount)} outcome rows · avg ${formatCompact(contextPackQuality.observedAverageProviderTotalTokens ?? 0)}/call`}
          tone={contextPackQuality.observedProviderUsageCount > 0 ? "good" : "neutral"}
        />
        <MetricCard
          label="runner advisor"
          value={runnerQuality.topRunner || "training"}
          detail={`${formatCompact(runnerQuality.sampleCount)} samples · ${runnerQuality.confidence.replace(/_/g, " ")}`}
          tone={runnerQuality.sampleCount >= 3 ? "good" : "neutral"}
          compactValue={!runnerQuality.topRunner}
        />
        <MetricCard
          label="service health"
          value={serviceHealth.label}
          detail="gateway, memory store, retrieval lanes"
          tone={serviceHealth.total && serviceHealth.healthy === serviceHealth.total ? "good" : "warn"}
        />
        <MetricCard
          label="memory write queue"
          value={`${formatCompact(toInt(memoryBank.queueDepth))}/${formatCompact(toInt(memoryBank.queueMax))}`}
          detail={`${formatCompact(toInt(memoryBank.processed))} processed · last ${formatMs(memoryTelemetry.lastWriteLatencyMs)}`}
          tone={toInt(memoryBank.queueDepth) === 0 ? "good" : "warn"}
        />
        <MetricCard
          label="fanout queue"
          value={`${formatCompact(toInt(fanout.queueDepth))}/${formatCompact(toInt(fanout.queueMax))}`}
          detail={`${formatCompact(toInt(fanout.processed))} processed · qdrant + pgvector`}
          tone={toInt(fanout.queueDepth) === 0 ? "good" : "warn"}
        />
        <MetricCard
          label="retrieval alerts"
          value={formatCompact(retrievalAlerts.length)}
          detail="latency, timeout, and source error pressure"
          tone={retrievalAlerts.length ? "warn" : "good"}
        />
        <MetricCard
          label="disk pressure"
          value={diskPressure}
          detail={`${toText(storageRecord.usedHuman) || "--"} used · ${toText(storageRecord.freeHuman) || "--"} free`}
          tone={toNumber(storageRecord.usedRatio) > 0.9 ? "warn" : "neutral"}
          compactValue={diskPressure.toLowerCase() === "tracked"}
        />
      </section>

      <AgentPacketWorkbench />

      <div className="cl-dashboard-grid">
        <section className="cl-panel cl-panel--wide">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">retrieval pressure</p>
              <h3>Lane latency by source</h3>
            </div>
            <span className="cl-badge">{sourceRows.length} lanes</span>
          </div>
          <div className="cl-lane-list">
            {sourceRows.length ? sourceRows.map((row) => <LaneBar key={row.source} name={row.source} metrics={row.metrics} />) : (
              <p className="cl-empty">No retrieval telemetry surfaced yet.</p>
            )}
          </div>
        </section>

        <section className="cl-panel">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">runner quality</p>
              <h3>Adapter advisor</h3>
            </div>
            <span className="cl-badge">{runnerQuality.mode.replace(/_/g, " ")}</span>
          </div>
          <div className="cl-impact-hero">
            <div>
              <span className="cl-label">samples</span>
              <strong>{formatCompact(runnerQuality.sampleCount)}</strong>
            </div>
            <div>
              <span className="cl-label">scope</span>
              <strong>{runnerQuality.taskClass.slice(0, 12)}</strong>
            </div>
            <div>
              <span className="cl-label">pick</span>
              <strong>{(runnerQuality.topRunner || "--").slice(0, 12)}</strong>
            </div>
          </div>
          <div className="cl-impact-stack">
            {runnerQuality.candidates.length ? runnerQuality.candidates.slice(0, 4).map((candidate) => (
              <div key={candidate.runner} className="cl-impact-row">
                <span>{candidate.runner}</span>
                <span>{formatPercent(candidate.successRate)} ok · {formatPercent(candidate.blockedRate)} blocked</span>
                <strong>{candidate.score}</strong>
              </div>
            )) : <p className="cl-empty">No adapter samples yet. Run task-agent workers through Pi, Droid, or another adapter to seed the ledger.</p>}
          </div>
          <div className="cl-impact-warnings">
            {runnerQuality.warnings.slice(0, 2).map((warning) => (
              <p key={warning}>{warning}</p>
            ))}
          </div>
          <p className="cl-panel-note">
            Advisor only: ContextLattice reports evidence; the operator chooses the runner.
          </p>
        </section>

        <section className="cl-panel">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">token impact engine</p>
              <h3>Prompt economics ledger</h3>
            </div>
            <span className="cl-badge">{tokenImpact.calibrationGrade.replace(/_/g, " ")}</span>
          </div>
          <div className="cl-impact-hero">
            <div>
              <span className="cl-label">compression</span>
              <strong>{tokenImpact.compressionRatio}x</strong>
            </div>
            <div>
              <span className="cl-label">quality</span>
              <strong>{tokenImpact.qualityScore}</strong>
            </div>
            <div>
              <span className="cl-label">16k windows</span>
              <strong>{tokenImpact.requestEquivalent}</strong>
            </div>
          </div>
          <div className="cl-impact-meter" aria-label="Token impact baseline and packed comparison">
            <div className="cl-impact-meter-row">
              <span>baseline</span>
              <div><i style={{ width: "100%" }} /></div>
              <strong>{formatCompact(tokenImpact.baselineTokens)}</strong>
            </div>
            <div className="cl-impact-meter-row cl-impact-meter-row--packed">
              <span>packed</span>
              <div>
                <i style={{ width: `${Math.max(4, Math.min(100, tokenImpact.baselineTokens > 0 ? (tokenImpact.packedTokens / tokenImpact.baselineTokens) * 100 : 0))}%` }} />
              </div>
              <strong>{formatCompact(tokenImpact.packedTokens)}</strong>
            </div>
          </div>
          <div className="cl-impact-stack">
            {tokenImpact.factors.map((item) => (
              <div key={`${item.role}-${item.label}`} className={`cl-impact-row cl-impact-row--${item.role}`}>
                <span>{item.label}</span>
                <span>{item.value}</span>
                <strong>{formatCompact(item.tokens)}</strong>
              </div>
            ))}
          </div>
          <div className="cl-impact-warnings">
            {tokenImpact.warnings.slice(0, 3).map((warning) => (
              <p key={warning}>{warning}</p>
            ))}
          </div>
          <p className="cl-panel-note">
            {tokenImpact.measurementLimit}
          </p>
        </section>

        <section className="cl-panel">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">topic heat</p>
              <h3>Most loaded branches</h3>
            </div>
            <a className="cl-text-link" href="/mindmap">Open Topics</a>
          </div>
          <div className="cl-topic-list">
            {topPaths.length ? topPaths.map((topic) => {
              const row = asRecord(topic);
              return (
                <div className="cl-topic-row" key={`${toText(row.path)}-${toText(row.project)}`}>
                  <span>{toText(row.path) || "root"}</span>
                  <strong>{formatCompact(toInt(row.eventCount))}</strong>
                </div>
              );
            }) : <p className="cl-empty">Topic rollups are still warming.</p>}
          </div>
        </section>

        <section className="cl-panel cl-panel--wide">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">active agents</p>
              <h3>Sessions carrying memory forward</h3>
            </div>
            <span className="cl-badge">{formatCompact(liveSessionCount)} live</span>
          </div>
          <div className="cl-session-list">
            {sessions.length ? sessions.map((session) => <SessionRow key={toText(asRecord(session).id)} session={session} />) : (
              <p className="cl-empty">No active agent sessions reported.</p>
            )}
          </div>
        </section>

        <section className="cl-panel">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">signals</p>
              <h3>What needs eyes</h3>
            </div>
          </div>
          <div className="cl-signal-list">
            {signals.length ? signals.map((signal, index) => (
              <div className="cl-signal-row" key={`${signal}-${index}`}>
                <span className="cl-signal-index">{String(index + 1).padStart(2, "0")}</span>
                <span>{signal}</span>
              </div>
            )) : <p className="cl-empty">No live warnings from dashboard sources.</p>}
          </div>
        </section>
      </div>
    </div>
  );
}
