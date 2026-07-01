"use client";

import { Suspense, useEffect, useRef, useState } from "react";
import { signIn } from "next-auth/react";
import { useRouter, useSearchParams } from "next/navigation";
import { dashboardClientAuthRequired } from "@/lib/authMode";
import { sanitizeCallbackUrl } from "@/lib/callback-url";

function ProviderMark({ provider }: { provider: string }) {
  const id = provider.toLowerCase();
  if (id === "github") {
    return <span className="auth-provider-glyph">GH</span>;
  }
  if (id === "google") {
    return <span className="auth-provider-glyph">GO</span>;
  }
  return <span className="auth-provider-glyph">ID</span>;
}

function LocalAuthDisabled() {
  return (
    <section className="auth-shell auth-shell--minimal">
      <div className="auth-panel auth-panel--local">
        <div className="auth-brand-row">
          <span className="auth-brand-pixel">CL</span>
          <span className="auth-brand-text">ContextLattice</span>
        </div>
        <p className="auth-kicker">Local dashboard</p>
        <h2 className="auth-title">No login required here.</h2>
        <p className="auth-subtitle">
          This open-source/local console is running with auth disabled. Billing and hosted account surfaces stay quiet; the operator dashboard is ready.
        </p>
        <div className="auth-local-actions">
          <a className="auth-submit auth-submit--link" href="/console">Open Console</a>
          <a className="auth-link auth-link--quiet" href="/downloads">Install + integrate agents</a>
        </div>
      </div>
    </section>
  );
}

function LoginPageContent() {
  const authRequired = dashboardClientAuthRequired();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [oauthProviders, setOauthProviders] = useState<Array<{ id: string; name: string }>>([]);
  const router = useRouter();
  const params = useSearchParams();
  const callbackUrl = sanitizeCallbackUrl(params.get("callbackUrl"));
  const providerHint = (params.get("provider") || "").toLowerCase();
  const providerAutoLaunchRef = useRef(false);

  useEffect(() => {
    if (!authRequired) return;
    fetch("/api/auth/providers")
      .then((res) => res.json())
      .then((providers) => {
        const list = Object.values(providers || {})
          .filter((provider: any) => provider.id !== "credentials")
          .map((provider: any) => ({ id: provider.id, name: provider.name }));
        setOauthProviders(list);
      })
      .catch(() => undefined);
  }, [authRequired]);

  useEffect(() => {
    if (!authRequired || providerAutoLaunchRef.current || !providerHint || oauthProviders.length === 0) return;
    const match = oauthProviders.find((provider) => provider.id === providerHint);
    if (!match) return;
    providerAutoLaunchRef.current = true;
    signIn(match.id, { callbackUrl });
  }, [authRequired, providerHint, oauthProviders, callbackUrl]);

  if (!authRequired) {
    return <LocalAuthDisabled />;
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setLoading(true);
    setError(null);
    const res = await signIn("credentials", {
      redirect: false,
      email,
      password,
      callbackUrl,
    });
    if (res?.error) {
      setError("Invalid email or password.");
      setLoading(false);
      return;
    }
    router.push(callbackUrl);
  }

  const providerHintMissing = providerHint && oauthProviders.length > 0 && !oauthProviders.find((provider) => provider.id === providerHint);

  return (
    <section className="auth-shell auth-shell--minimal">
      <div className="auth-panel auth-panel--login">
        <header className="auth-header">
          <div className="auth-brand-row" aria-label="ContextLattice">
            <span className="auth-brand-pixel">CL</span>
            <span className="auth-brand-text">ContextLattice</span>
          </div>
          <p className="auth-kicker">Hosted access</p>
          <h2 className="auth-title">Enter the control room.</h2>
          <p className="auth-subtitle">Sign in only when this deployment has hosted billing, account, or paid-runtime lanes enabled.</p>
        </header>

        <form onSubmit={handleSubmit} className="auth-form">
          <label className="auth-field">
            <span className="auth-label">Email</span>
            <input className="auth-input" type="email" value={email} onChange={(e) => setEmail(e.target.value)} required />
          </label>
          <label className="auth-field">
            <span className="auth-label">Password</span>
            <input className="auth-input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required />
          </label>
          {error ? <p className="auth-error">{error}</p> : null}
          <button type="submit" className="auth-submit" disabled={loading}>{loading ? "Checking" : "Sign in"}</button>
        </form>

        {oauthProviders.length ? (
          <>
            <div className="auth-divider"><span>or</span></div>
            <div className="auth-provider-grid">
              {oauthProviders.map((provider) => (
                <button key={provider.id} className="auth-provider" onClick={() => signIn(provider.id, { callbackUrl })} type="button">
                  <ProviderMark provider={provider.id} />
                  <span>{provider.name}</span>
                </button>
              ))}
            </div>
          </>
        ) : null}

        {providerHintMissing ? <p className="auth-inline-warning">Requested provider is not configured on this deployment.</p> : null}
        <p className="auth-meta"><a className="auth-link" href="/auth/request-reset">Reset password</a> · <a className="auth-link" href="/auth/register">Create account</a></p>
      </div>
    </section>
  );
}

export default function LoginPage() {
  return (
    <Suspense fallback={<section className="auth-shell"><div className="auth-panel">Loading sign-in...</div></section>}>
      <LoginPageContent />
    </Suspense>
  );
}
