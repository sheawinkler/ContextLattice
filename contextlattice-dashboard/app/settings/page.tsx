"use client";

import { useEffect, useState } from "react";

type Workspace = {
  id: string;
  name: string;
  slug: string;
  role: string;
  status?: string;
};

type ApiKey = {
  id: string;
  name: string;
  prefix: string;
  scopes?: string | null;
  createdAt: string;
  lastUsedAt?: string | null;
};

type Budget = {
  tokenLimit?: number | null;
  costLimitUsd?: number | null;
};

type UsageSummary = {
  tokens: number;
  costUsd: number;
};

type AuditLog = {
  id: string;
  action: string;
  targetType?: string | null;
  targetId?: string | null;
  createdAt: string;
};

function formatDate(value?: string | null) {
  if (!value) return "--";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "--" : date.toLocaleString();
}

function SecretOutput({ label, value }: { label: string; value: string }) {
  return (
    <div className="cl-secret-card">
      <span className="cl-label">{label}</span>
      <p>Copy now. This value is only shown once.</p>
      <code>{value}</code>
    </div>
  );
}

export default function SettingsPage() {
  const [workspace, setWorkspace] = useState<Workspace | null>(null);
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [newKeyName, setNewKeyName] = useState("");
  const [newKeyScopes, setNewKeyScopes] = useState("");
  const [newKeyValue, setNewKeyValue] = useState<string | null>(null);
  const [keyLimit, setKeyLimit] = useState<number | null>(null);
  const [planId, setPlanId] = useState<string | null>(null);
  const [budget, setBudget] = useState<Budget | null>(null);
  const [usage, setUsage] = useState<UsageSummary | null>(null);
  const [tokenLimit, setTokenLimit] = useState<string>("");
  const [costLimit, setCostLimit] = useState<string>("");
  const [message, setMessage] = useState<string | null>(null);
  const [auditLogs, setAuditLogs] = useState<AuditLog[]>([]);

  async function loadWorkspace() {
    const res = await fetch("/api/workspace/current");
    if (!res.ok) return;
    const data = await res.json();
    setWorkspace(data.workspace);
  }

  async function loadKeys() {
    const res = await fetch("/api/workspace/api-keys");
    if (!res.ok) return;
    const data = await res.json();
    setKeys(data.keys || []);
    setKeyLimit(
      data.limit === null || typeof data.limit === "number" ? data.limit : null,
    );
    setPlanId(typeof data.planId === "string" ? data.planId : null);
  }

  async function loadBudget() {
    const res = await fetch("/api/workspace/budget");
    if (!res.ok) return;
    const data = await res.json();
    setBudget(data.budget || null);
    setUsage(data.usage || null);
  }

  async function loadAuditLogs() {
    const res = await fetch("/api/workspace/audit?limit=20");
    if (!res.ok) return;
    const data = await res.json();
    setAuditLogs(data.logs || []);
  }

  useEffect(() => {
    void loadWorkspace();
    void loadKeys();
    void loadBudget();
    void loadAuditLogs();
  }, []);

  async function createKey() {
    const name = newKeyName.trim();
    if (!name) {
      setMessage("Name the key before creating it.");
      return;
    }
    setMessage(null);
    setNewKeyValue(null);
    const res = await fetch("/api/workspace/api-keys", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name, scopes: newKeyScopes.trim() || undefined }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Failed to create key");
      return;
    }
    setNewKeyValue(data.key?.apiKey || null);
    setNewKeyName("");
    setNewKeyScopes("");
    void loadKeys();
  }

  async function revokeKey(id: string) {
    setMessage(null);
    const res = await fetch(`/api/workspace/api-keys/${id}`, { method: "DELETE" });
    if (!res.ok) {
      setMessage("Failed to revoke key");
      return;
    }
    void loadKeys();
  }

  async function saveBudget() {
    setMessage(null);
    const tokenLimitNum = tokenLimit ? Number(tokenLimit) : null;
    const costLimitNum = costLimit ? Number(costLimit) : null;
    const res = await fetch("/api/workspace/budget", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        tokenLimit: Number.isFinite(tokenLimitNum) ? tokenLimitNum : null,
        costLimitUsd: Number.isFinite(costLimitNum) ? costLimitNum : null,
      }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Failed to save budget");
      return;
    }
    setBudget(data.budget);
    setTokenLimit("");
    setCostLimit("");
  }

  async function exportWorkspace() {
    setMessage(null);
    const res = await fetch("/api/workspace/export");
    if (!res.ok) {
      setMessage("Failed to export workspace");
      return;
    }
    const data = await res.json();
    const blob = new Blob([JSON.stringify(data, null, 2)], {
      type: "application/json",
    });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = `contextlattice-export-${Date.now()}.json`;
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
  }

  async function requestDeletion() {
    setMessage(null);
    if (!confirm("This will request workspace deletion and revoke API keys. Continue?")) {
      return;
    }
    const res = await fetch("/api/workspace/delete", { method: "POST" });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Failed to request deletion");
      return;
    }
    setMessage("Deletion request submitted.");
    void loadWorkspace();
    void loadKeys();
    void loadAuditLogs();
  }

  return (
    <div className="cl-page cl-settings-page">
      <section className="cl-hero cl-hero--compact">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Settings // control surface</p>
          <h2>Keys, limits, exports, and audit truth.</h2>
          <p>
            Account controls without filler: issue explicit keys, set budget rails, export workspace state,
            and inspect the trail of administrative actions.
          </p>
        </div>
        <div className="cl-overview-stamp">
          <span>workspace</span>
          <strong>{workspace?.role || "signed out"}</strong>
          <small>{workspace?.slug || "local dashboard"}</small>
        </div>
      </section>

      {message ? <section className="cl-alert-strip"><strong>Settings notice.</strong><span>{message}</span></section> : null}

      <section className="cl-settings-grid">
        <article className="cl-panel cl-settings-card cl-settings-card--identity">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">workspace</p>
              <h3>{workspace?.name || "Sign in to manage"}</h3>
            </div>
            <span className="cl-badge">{workspace?.status || workspace?.role || "local"}</span>
          </div>
          {workspace ? (
            <div className="cl-definition-grid">
              <span>Name</span><strong>{workspace.name}</strong>
              <span>Slug</span><strong>{workspace.slug}</strong>
              <span>Role</span><strong>{workspace.role}</strong>
              {workspace.status ? <><span>Status</span><strong>{workspace.status}</strong></> : null}
            </div>
          ) : (
            <p className="cl-panel-note">Sign in when this deployment has account mode enabled. Local OSS mode does not require dashboard auth.</p>
          )}
        </article>

        <article className="cl-panel cl-settings-card">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">runtime access</p>
              <h3>API keys</h3>
            </div>
            <span className="cl-badge">{planId ? `${keys.length}/${keyLimit === null ? "unlimited" : keyLimit}` : "locked"}</span>
          </div>
          <p className="cl-panel-note">Keys are shown once. Add only the scopes this integration actually needs.</p>
          <div className="cl-form-grid">
            <label className="cl-field">
              <span>Key name</span>
              <input value={newKeyName} onChange={(e) => setNewKeyName(e.target.value)} placeholder="example: CI write key" />
            </label>
            <label className="cl-field">
              <span>Scopes</span>
              <input value={newKeyScopes} onChange={(e) => setNewKeyScopes(e.target.value)} placeholder="memory:write,usage:write" />
            </label>
            <button className="cl-button" onClick={createKey} disabled={!newKeyName.trim()} type="button">Create key</button>
          </div>
          {newKeyValue ? <SecretOutput label="new api key" value={newKeyValue} /> : null}
          <div className="cl-settings-list">
            {keys.length === 0 ? <p className="cl-empty">No keys yet.</p> : keys.map((key) => (
              <div className="cl-settings-row" key={key.id}>
                <div>
                  <strong>{key.name}</strong>
                  <span>{key.prefix}... / {key.scopes || "default scopes"} / created {formatDate(key.createdAt)}</span>
                </div>
                <button className="cl-text-button" onClick={() => revokeKey(key.id)} type="button">Revoke</button>
              </div>
            ))}
          </div>
        </article>

        <article className="cl-panel cl-settings-card">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">budget rails</p>
              <h3>Usage budgets</h3>
            </div>
            <span className="cl-badge">monthly</span>
          </div>
          <p className="cl-panel-note">Budget inputs are explicit. Blank fields remove that limit.</p>
          <div className="cl-form-grid">
            <label className="cl-field">
              <span>Token limit</span>
              <input inputMode="numeric" value={tokenLimit} onChange={(e) => setTokenLimit(e.target.value)} placeholder="blank = no token cap" />
            </label>
            <label className="cl-field">
              <span>Cost limit USD</span>
              <input inputMode="decimal" value={costLimit} onChange={(e) => setCostLimit(e.target.value)} placeholder="blank = no cost cap" />
            </label>
            <button className="cl-button cl-button--secondary" onClick={saveBudget} type="button">Save budget</button>
          </div>
          <div className="cl-definition-grid">
            <span>Active</span><strong>{budget ? `${budget.tokenLimit ?? "no token cap"} / ${budget.costLimitUsd ?? "no cost cap"} USD` : "none"}</strong>
            <span>Month to date</span><strong>{usage ? `${usage.tokens} tokens / $${usage.costUsd.toFixed(2)}` : "--"}</strong>
          </div>
        </article>

        <article className="cl-panel cl-settings-card">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">portability</p>
              <h3>Data export</h3>
            </div>
          </div>
          <p className="cl-panel-note">Export workspace metadata, usage, and audit logs. Memory bank files are exported from the memory store separately.</p>
          <button className="cl-button cl-button--secondary" onClick={exportWorkspace} type="button">Download export JSON</button>
        </article>

        <article className="cl-panel cl-settings-card cl-settings-card--danger">
          <div className="cl-section-head">
            <div>
              <p className="cl-kicker">danger rail</p>
              <h3>Deletion request</h3>
            </div>
          </div>
          <p className="cl-panel-note">Request workspace deletion and revoke active API keys immediately.</p>
          <button className="cl-button cl-button--danger" onClick={requestDeletion} type="button">Request deletion</button>
        </article>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">audit</p>
            <h3>Administrative trail</h3>
          </div>
          <span className="cl-badge">{auditLogs.length} events</span>
        </div>
        <div className="cl-settings-list cl-settings-list--audit">
          {auditLogs.length === 0 ? <p className="cl-empty">No audit events yet.</p> : auditLogs.map((log) => (
            <div className="cl-settings-row" key={log.id}>
              <div>
                <strong>{log.action}</strong>
                <span>{log.targetType || "system"} / {formatDate(log.createdAt)}</span>
              </div>
              <code>{log.targetId ? log.targetId.slice(0, 8) : "--"}</code>
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}
