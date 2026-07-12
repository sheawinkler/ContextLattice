"use client";

import { FormEvent, useState } from "react";
import { asArray, asRecord, formatCompact, toNumber, toText } from "@/lib/dashboardMetrics";

type AgentPacketWorkbenchProps = {
  initialPacket?: any;
};

function gateTone(decision: string): string {
  if (decision === "act") return "act";
  if (decision === "verify") return "verify";
  return "abstain";
}

export function AgentPacketView({ packet }: { packet: any }) {
  const gate = asRecord(packet?.decision_gate);
  const uncertainty = asRecord(packet?.uncertainty);
  const tokenBudget = asRecord(packet?.token_budget);
  const tokenImpact = asRecord(packet?.token_impact);
  const evidence = asArray(packet?.evidence).slice(0, 8);
  const actions = asArray(packet?.next_actions).slice(0, 4);
  const decision = toText(gate.decision) || "abstain";

  const copyText = async (value: string) => {
    if (value && navigator.clipboard) await navigator.clipboard.writeText(value);
  };

  return (
    <div className="cl-packet-result" data-testid="agent-packet-result">
      <div className="cl-packet-verdict">
        <div>
          <span className="cl-label">decision gate</span>
          <strong className={`cl-gate cl-gate--${gateTone(decision)}`}>{decision}</strong>
        </div>
        <div>
          <span className="cl-label">uncertainty</span>
          <strong>{toText(uncertainty.status) || "unknown"}</strong>
        </div>
        <div>
          <span className="cl-label">wire tokens</span>
          <strong>{formatCompact(tokenBudget.actual_tokens)}</strong>
        </div>
        <div>
          <span className="cl-label">net delta</span>
          <strong>{formatCompact(toNumber(tokenImpact.net_token_delta))}</strong>
        </div>
      </div>

      <div className="cl-packet-columns">
        <div className="cl-packet-evidence">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">evidence</p>
              <h3>What the lattice can prove</h3>
            </div>
            <span className="cl-badge">{evidence.length} selected</span>
          </div>
          {evidence.length ? evidence.map((item, index) => {
            const row = asRecord(item);
            return (
              <div className="cl-packet-evidence-row" key={`${toText(row.citation)}-${index}`}>
                <span className="cl-packet-rank">{String(index + 1).padStart(2, "0")}</span>
                <div>
                  <p>{toText(row.text)}</p>
                  <small>{toText(row.citation) || [toText(row.project), toText(row.file)].filter(Boolean).join(" / ") || "uncited"}</small>
                </div>
                <span className="cl-packet-score">{Math.round(toNumber(row.relevance) * 100)}%</span>
              </div>
            );
          }) : <p className="cl-empty">No bounded evidence matched. The packet correctly abstained.</p>}
        </div>

        <div className="cl-packet-actions">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">next move</p>
              <h3>Copy, never auto-run</h3>
            </div>
          </div>
          <p className="cl-panel-note">ContextLattice proposes bounded inspection steps. This dashboard executes nothing.</p>
          {actions.length ? actions.map((item, index) => {
            const row = asRecord(item);
            const command = toText(row.command);
            return (
              <div className="cl-packet-action-row" key={`${toText(row.label)}-${index}`}>
                <div>
                  <strong>{toText(row.label) || `action ${index + 1}`}</strong>
                  {command ? <code>{command}</code> : null}
                  {toText(row.reason) ? <small>{toText(row.reason)}</small> : null}
                </div>
                {command ? <button type="button" className="cl-copy-button" onClick={() => void copyText(command)}>Copy</button> : null}
              </div>
            );
          }) : <p className="cl-empty">No action is justified by the current evidence.</p>}
        </div>
      </div>
    </div>
  );
}

export function AgentPacketWorkbench({ initialPacket = null }: AgentPacketWorkbenchProps) {
  const [query, setQuery] = useState("");
  const [project, setProject] = useState("contextlattice");
  const [packet, setPacket] = useState<any>(initialPacket);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!query.trim()) return;
    setLoading(true);
    setError("");
    try {
      const response = await fetch("/api/memory/agent-packet", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ query: query.trim(), project: project.trim() || "contextlattice" }),
      });
      const body = await response.json().catch(() => null);
      if (!response.ok) throw new Error(toText(body?.detail) || toText(body?.error) || `HTTP ${response.status}`);
      setPacket(body);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setLoading(false);
    }
  };

  return (
    <section className="cl-panel cl-agent-workbench">
      <div className="cl-section-head">
        <div>
          <p className="cl-kicker">truth / action</p>
          <h3>Ask the lattice. See its proof.</h3>
        </div>
        <span className="cl-badge">agent_packet.v1</span>
      </div>
      <form className="cl-packet-form" onSubmit={submit}>
        <label>
          <span>Task or decision</span>
          <input value={query} onChange={(event) => setQuery(event.target.value)} maxLength={1600} placeholder="What can the evidence prove about this task?" />
        </label>
        <label>
          <span>Project</span>
          <input value={project} onChange={(event) => setProject(event.target.value)} maxLength={120} />
        </label>
        <button className="cl-button" type="submit" disabled={loading || !query.trim()}>{loading ? "Retrieving" : "Build proof"}</button>
      </form>
      {error ? <div className="cl-alert-strip"><strong>Packet unavailable.</strong><span>{error}</span></div> : null}
      {packet ? <AgentPacketView packet={packet} /> : <p className="cl-panel-note">Compact by default. Evidence is deduplicated, provenance stays visible, and weak support produces verify or abstain instead of fake certainty.</p>}
    </section>
  );
}
