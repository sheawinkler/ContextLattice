"use client";

import { useEffect, useMemo, useState } from "react";
import { useSession } from "next-auth/react";
import { dashboardClientAuthRequired } from "@/lib/authMode";

type DownloadAsset = {
  id: string;
  label: string;
  fileName: string;
  route: string;
};

type DownloadCatalog = {
  planId: string;
  expiresInMinutesDefault: number;
  assets: DownloadAsset[];
};

type TokenLink = {
  id: string;
  label: string;
  fileName: string;
  expiresInMinutes: number;
  url: string;
};

type DashboardSessionLike = {
  user?: { email?: string | null } | null;
} | null;

const AGENT_PROMPT = `Navigate to github.com/sheawinkler/contextlattice, install ContextLattice using the documented quickstart, run the doctor, then integrate only with agent harnesses that are actually installed on this machine. Configure hooks/agent files for the detected agents, verify ContextLattice recall/writeback works, and report exact commands plus any skipped integrations.`;

function CopyableCommand({ label, command }: { label: string; command: string }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    await navigator.clipboard.writeText(command);
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1200);
  }
  return (
    <div className="cl-command-card">
      <div>
        <span className="cl-label">{label}</span>
        <code>{command}</code>
      </div>
      <button className="cl-button" type="button" onClick={copy}>{copied ? "Copied" : "Copy"}</button>
    </div>
  );
}

function HostedArtifacts({ authRequired, session }: { authRequired: boolean; session: DashboardSessionLike }) {
  const [catalog, setCatalog] = useState<DownloadCatalog | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [tokenLinks, setTokenLinks] = useState<TokenLink[]>([]);
  const [issuedKey, setIssuedKey] = useState<string | null>(null);
  const [issuedPrefix, setIssuedPrefix] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    if (!authRequired) {
      setMessage("Hosted paid artifacts are disabled in local OSS mode.");
      return;
    }
    let mounted = true;
    fetch("/api/billing/downloads", { cache: "no-store" })
      .then(async (res) => ({ status: res.status, data: await res.json() }))
      .then(({ status, data }) => {
        if (!mounted) return;
        if (status === 402) {
          setMessage(data?.error || "Active subscription required for premium artifacts.");
          return;
        }
        if (status === 401) {
          setMessage(authRequired ? "Sign in to access hosted artifacts." : "Hosted paid artifacts are disabled in local OSS mode.");
          return;
        }
        if (!data?.ok) {
          setMessage(data?.error || "Hosted artifact catalog is not available here.");
          return;
        }
        setCatalog(data);
      })
      .catch(() => {
        if (mounted) setMessage("Hosted artifact catalog is not available here.");
      });
    return () => {
      mounted = false;
    };
  }, [authRequired]);

  const hasAssets = useMemo(() => (catalog?.assets?.length || 0) > 0, [catalog]);
  const canIssueRuntimeKey = authRequired && !!session?.user?.email && !!catalog?.planId;

  async function issueTimedLinks(email: boolean) {
    setBusy(email ? "email-links" : "links");
    setMessage(null);
    try {
      const res = await fetch("/api/billing/download-token", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ ttlMinutes: catalog?.expiresInMinutesDefault || 120, email }),
      });
      const data = await res.json();
      if (!res.ok) {
        setMessage(data?.error || "Failed to issue download links.");
        return;
      }
      setTokenLinks(data.links || []);
      setMessage(email ? "Timed links generated; email delivery attempted." : "Timed download links generated.");
    } finally {
      setBusy(null);
    }
  }

  async function issueRuntimeKey() {
    setBusy("runtime-key");
    setMessage(null);
    try {
      const res = await fetch("/api/billing/entitlement/issue", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ rotate: false }),
      });
      const data = await res.json();
      if (!res.ok) {
        setMessage(data?.error || "Failed to issue runtime key.");
        return;
      }
      setIssuedKey(data?.key?.apiKey || null);
      setIssuedPrefix(data?.key?.prefix || null);
      setMessage("Premium runtime key issued. Store it now; it is shown once.");
    } finally {
      setBusy(null);
    }
  }

  return (
    <section className="cl-panel">
      <div className="cl-section-head">
        <div>
          <p className="cl-kicker">hosted lane</p>
          <h3>Paid artifacts and runtime keys</h3>
        </div>
        <span className="cl-badge">{authRequired ? "auth enabled" : "local mode"}</span>
      </div>
      <p className="cl-panel-note">
        This section only matters for hosted/paid deployments. Local open-source dashboard usage does not require login or premium downloads.
      </p>
      {session?.user?.email ? <p className="cl-panel-note">Signed in as {session.user.email}</p> : null}
      {hasAssets ? (
        <div className="cl-artifact-grid">
          {catalog!.assets.map((asset) => (
            <a key={asset.id} className="cl-artifact-card" href={asset.route}>
              <strong>{asset.label}</strong>
              <span>{asset.fileName}</span>
            </a>
          ))}
        </div>
      ) : null}
      <div className="cl-button-row">
        <button className="cl-button" disabled={!hasAssets || busy !== null} onClick={() => issueTimedLinks(false)} type="button">
          {busy === "links" ? "Generating" : "Generate links"}
        </button>
        <button className="cl-button" disabled={!hasAssets || busy !== null} onClick={() => issueTimedLinks(true)} type="button">
          {busy === "email-links" ? "Emailing" : "Generate + email"}
        </button>
        <button className="cl-button" disabled={!canIssueRuntimeKey || busy !== null} onClick={issueRuntimeKey} type="button">
          {busy === "runtime-key" ? "Issuing" : "Issue runtime key"}
        </button>
      </div>
      {!canIssueRuntimeKey ? (
        <p className="cl-panel-note">Runtime keys are issued only to signed-in users with an active paid entitlement.</p>
      ) : null}
      {tokenLinks.length ? (
        <div className="cl-link-stack">
          {tokenLinks.map((link) => (
            <a key={link.id} href={link.url}>{link.label} · expires in {link.expiresInMinutes}m</a>
          ))}
        </div>
      ) : null}
      {issuedPrefix ? <p className="cl-panel-note">Key prefix: <code>{issuedPrefix}</code></p> : null}
      {issuedKey ? <code className="cl-secret-output">{issuedKey}</code> : null}
      {message ? <p className="cl-panel-note">{message}</p> : null}
    </section>
  );
}

function InstallContent({ authRequired, session }: { authRequired: boolean; session: DashboardSessionLike }) {
  return (
    <div className="cl-page cl-install-page">
      <section className="cl-hero cl-hero--compact">
        <div className="cl-hero-copy">
          <p className="cl-kicker">Install // local agent memory</p>
          <h2>Turn the machine on. Then let agents plug in.</h2>
          <p>
            This page is not a shopping mall. It is the local install and integration desk: CLI, agent prompt, doctor, and hosted artifact lane only when that deployment supports it.
          </p>
        </div>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">fast path</p>
            <h3>Install and verify</h3>
          </div>
          <a className="cl-text-link" href="https://github.com/sheawinkler/contextlattice" target="_blank" rel="noreferrer">GitHub</a>
        </div>
        <div className="cl-command-grid">
          <CopyableCommand label="clone" command="git clone https://github.com/sheawinkler/contextlattice.git" />
          <CopyableCommand label="enter" command="cd contextlattice" />
          <CopyableCommand label="doctor" command="contextlattice doctor" />
          <CopyableCommand label="agent integration" command="contextlattice_agent_integrate --detect --doctor" />
        </div>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">agent prompt</p>
            <h3>For any agent already reading this page</h3>
          </div>
          <button className="cl-button" type="button" onClick={() => navigator.clipboard.writeText(AGENT_PROMPT)}>Copy prompt</button>
        </div>
        <pre className="cl-prompt-box">{AGENT_PROMPT}</pre>
        <p className="cl-panel-note">
          The integration flow should detect installed harnesses and skip missing ones. It should not invent agent configs for tools that are not present.
        </p>
      </section>

      <section className="cl-panel">
        <div className="cl-section-head">
          <div>
            <p className="cl-kicker">local surfaces</p>
            <h3>What gets wired</h3>
          </div>
        </div>
        <div className="cl-surface-grid">
          <article><strong>CLI</strong><span>write, search, policy pack, checkpoint, doctor</span></article>
          <article><strong>Hooks</strong><span>pre/post compaction, handoff, lifecycle events</span></article>
          <article><strong>Agent files</strong><span>AGENTS.md, CLAUDE.md style guidance, harness-specific profiles where detected</span></article>
          <article><strong>Memory lanes</strong><span>topic rollups, qdrant, pgvector, raw ledger, deep recall</span></article>
        </div>
      </section>

      <HostedArtifacts authRequired={authRequired} session={session} />
    </div>
  );
}

function InstallWithSession() {
  const { data: session } = useSession();
  return <InstallContent authRequired session={session} />;
}

export function InstallArtifactsPage() {
  if (dashboardClientAuthRequired()) {
    return <InstallWithSession />;
  }
  return <InstallContent authRequired={false} session={null} />;
}
