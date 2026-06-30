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
  user?: {
    email?: string | null;
  } | null;
} | null;

function DownloadsPageContent({
  authRequired,
  session,
}: {
  authRequired: boolean;
  session: DashboardSessionLike;
}) {
  const [catalog, setCatalog] = useState<DownloadCatalog | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const [tokenLinks, setTokenLinks] = useState<TokenLink[]>([]);
  const [issuedKey, setIssuedKey] = useState<string | null>(null);
  const [issuedPrefix, setIssuedPrefix] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    let mounted = true;
    fetch("/api/billing/downloads")
      .then(async (res) => ({ status: res.status, data: await res.json() }))
      .then(({ status, data }) => {
        if (!mounted) return;
        if (status === 402) {
          setMessage(data?.error || "Active subscription required for premium downloads.");
          return;
        }
        if (status === 401) {
          setMessage(
            authRequired
              ? "Sign in to access premium downloads."
              : "Premium downloads require a hosted/authenticated dashboard. Local open-source dashboard access does not require sign-in.",
          );
          return;
        }
        if (!data?.ok) {
          setMessage(data?.error || "Failed to load premium artifacts.");
          return;
        }
        setCatalog(data);
      })
      .catch(() => {
        if (mounted) {
          setMessage("Failed to load premium artifacts.");
        }
      });
    return () => {
      mounted = false;
    };
  }, [authRequired]);

  const hasAssets = useMemo(() => (catalog?.assets?.length || 0) > 0, [catalog]);

  async function issueTimedLinks(email: boolean) {
    setBusy(email ? "email-links" : "links");
    setMessage(null);
    try {
      const res = await fetch("/api/billing/download-token", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          ttlMinutes: catalog?.expiresInMinutesDefault || 120,
          email,
        }),
      });
      const data = await res.json();
      if (!res.ok) {
        setMessage(data?.error || "Failed to issue download links.");
        return;
      }
      setTokenLinks(data.links || []);
      if (email) {
        if (data?.email?.ok) {
          setMessage("Timed links were generated and emailed.");
        } else {
          setMessage(
            `Timed links generated. Email failed: ${data?.email?.error || "SMTP not configured."}`,
          );
        }
      } else {
        setMessage("Timed download links generated.");
      }
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
      if (data?.email?.ok) {
        setMessage("Premium runtime key issued and emailed.");
      } else {
        setMessage("Premium runtime key issued. Store it now; it is shown once.");
      }
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="space-y-6">
      <section className="card">
        <h2 className="text-xl font-semibold">Premium Downloads</h2>
        <p className="text-sm text-slate-400 mt-1">
          Final step: after checkout on <a className="underline" href="/billing">Billing</a>, download paid artifacts here.
        </p>
        <div className="mt-3 text-sm text-slate-300 space-y-1">
          <p><strong>Step 4 of 4:</strong> pull artifacts and issue runtime key.</p>
          <p>Use timed links for secure sharing with entitled users.</p>
        </div>
        {!session?.user ? (
          <p className="text-sm text-amber-300 mt-2">
            {authRequired ? (
              <>
                <a className="underline" href="/auth/login">Sign in</a> to access premium downloads.
              </>
            ) : (
              "Local open-source dashboard mode does not require sign-in. Premium downloads stay locked unless hosted auth is enabled."
            )}
          </p>
        ) : (
          <p className="text-sm text-emerald-300 mt-2">
            Signed in as {session.user.email}
          </p>
        )}
        {catalog?.planId ? (
          <p className="text-xs text-slate-400 mt-2">
            Active plan: <span className="font-semibold">{catalog.planId}</span>
          </p>
        ) : null}
      </section>

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">Direct Downloads</h3>
        {!hasAssets ? (
          <p className="text-sm text-slate-400">
            Premium artifacts are not available yet for this account/environment. Confirm active billing on <a className="underline" href="/billing">Billing</a>.
          </p>
        ) : (
          <div className="grid md:grid-cols-3 gap-3">
            {catalog!.assets.map((asset) => (
              <a
                key={asset.id}
                className="rounded border border-slate-700 px-3 py-3 hover:border-emerald-400"
                href={asset.route}
              >
                <div className="font-semibold">{asset.label}</div>
                <div className="text-xs text-slate-400 mt-1">{asset.fileName}</div>
              </a>
            ))}
          </div>
        )}
      </section>

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">Timed Links</h3>
        <p className="text-sm text-slate-400">
          Generate expiring links for secure sharing with entitled users.
        </p>
        <div className="flex flex-wrap gap-2">
          <button
            className="rounded border border-slate-700 px-4 py-2"
            disabled={!hasAssets || busy !== null}
            onClick={() => issueTimedLinks(false)}
          >
            {busy === "links" ? "Generating..." : "Generate timed links"}
          </button>
          <button
            className="rounded border border-slate-700 px-4 py-2"
            disabled={!hasAssets || busy !== null}
            onClick={() => issueTimedLinks(true)}
          >
            {busy === "email-links" ? "Emailing..." : "Generate + email links"}
          </button>
        </div>
        {tokenLinks.length ? (
          <div className="space-y-2">
            {tokenLinks.map((link) => (
              <div key={link.id} className="rounded border border-slate-800 px-3 py-2">
                <div className="text-sm font-semibold">{link.label}</div>
                <a className="text-xs text-emerald-300 break-all" href={link.url}>
                  {link.url}
                </a>
                <div className="text-xs text-slate-400 mt-1">
                  Expires in {link.expiresInMinutes} minute(s)
                </div>
              </div>
            ))}
          </div>
        ) : null}
      </section>

      <section className="card space-y-3">
        <h3 className="text-lg font-semibold">Premium Runtime Key</h3>
        <p className="text-sm text-slate-400">
          Issue a premium API key for authenticated paid-runtime usage on private lanes.
        </p>
        <button
          className="rounded border border-slate-700 px-4 py-2"
          disabled={busy !== null}
          onClick={issueRuntimeKey}
        >
          {busy === "runtime-key" ? "Issuing..." : "Issue runtime key"}
        </button>
        {issuedPrefix ? (
          <p className="text-xs text-slate-400">
            Key prefix: <code>{issuedPrefix}</code>
          </p>
        ) : null}
        {issuedKey ? (
          <div className="rounded border border-emerald-800/60 bg-emerald-950/30 px-3 py-2">
            <p className="text-xs text-emerald-200 mb-1">Store this key now (shown once):</p>
            <code className="text-xs break-all text-emerald-100">{issuedKey}</code>
          </div>
        ) : null}
      </section>

      {message ? (
        <section className="card">
          <h3 className="text-lg font-semibold">Status</h3>
          <p className="text-sm text-slate-300">{message}</p>
        </section>
      ) : null}
    </div>
  );
}

function DownloadsPageWithSession() {
  const { data: session } = useSession();
  return <DownloadsPageContent authRequired session={session} />;
}

export default function DownloadsPage() {
  if (dashboardClientAuthRequired()) {
    return <DownloadsPageWithSession />;
  }
  return <DownloadsPageContent authRequired={false} session={null} />;
}
