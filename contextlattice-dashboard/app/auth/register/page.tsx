"use client";

import { useEffect, useState } from "react";
import { signIn } from "next-auth/react";
import { useRouter } from "next/navigation";

function GithubMark() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className="auth-provider-icon">
      <path
        fill="currentColor"
        d="M12 .5A11.5 11.5 0 0 0 .5 12.3c0 5.26 3.44 9.73 8.2 11.3.6.12.82-.26.82-.58v-2.03c-3.33.75-4.03-1.46-4.03-1.46-.55-1.42-1.33-1.79-1.33-1.79-1.08-.76.08-.74.08-.74 1.2.09 1.83 1.26 1.83 1.26 1.06 1.86 2.78 1.32 3.46 1 .11-.8.41-1.33.75-1.64-2.66-.31-5.46-1.37-5.46-6.1 0-1.35.47-2.45 1.24-3.31-.13-.31-.54-1.57.12-3.26 0 0 1.02-.33 3.35 1.26a11.3 11.3 0 0 1 6.1 0c2.33-1.6 3.35-1.26 3.35-1.26.66 1.69.24 2.95.12 3.26.77.86 1.24 1.96 1.24 3.31 0 4.74-2.8 5.78-5.47 6.09.43.38.81 1.11.81 2.25v3.33c0 .32.21.7.82.58a11.8 11.8 0 0 0 8.2-11.3A11.5 11.5 0 0 0 12 .5Z"
      />
    </svg>
  );
}

function GoogleMark() {
  return (
    <svg viewBox="0 0 18 18" aria-hidden="true" className="auth-provider-icon">
      <path fill="#EA4335" d="M9.12 7.38v3.3h4.59c-.2 1.06-.8 1.96-1.7 2.57l2.74 2.13c1.6-1.48 2.52-3.65 2.52-6.21 0-.6-.05-1.16-.16-1.7H9.12Z" />
      <path fill="#34A853" d="M9.12 17.5c2.29 0 4.21-.75 5.62-2.04l-2.74-2.13c-.76.51-1.73.82-2.88.82-2.22 0-4.1-1.5-4.77-3.5l-2.84 2.19A8.5 8.5 0 0 0 9.12 17.5Z" />
      <path fill="#4A90E2" d="M4.35 10.66A5.12 5.12 0 0 1 4.07 9c0-.58.1-1.13.28-1.66L1.5 5.14A8.52 8.52 0 0 0 .62 9c0 1.39.33 2.7.89 3.86l2.84-2.2Z" />
      <path fill="#FBBC05" d="M9.12 3.85c1.24 0 2.36.43 3.23 1.28l2.43-2.43A8.1 8.1 0 0 0 9.12.5c-3.33 0-6.2 1.9-7.62 4.64l2.84 2.2c.66-2 2.55-3.5 4.77-3.5Z" />
    </svg>
  );
}

export default function RegisterPage() {
  const [name, setName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [oauthProviders, setOauthProviders] = useState<
    Array<{ id: string; name: string }>
  >([]);
  const fallbackProviders = [
    { id: "github", name: "GitHub" },
    { id: "google", name: "Google" },
  ];
  const router = useRouter();

  function providerClass(providerId: string, enabled: boolean) {
    const id = providerId.toLowerCase();
    if (!enabled) {
      return "auth-provider auth-provider--disabled";
    }
    if (id === "github") {
      return "auth-provider auth-provider--github";
    }
    if (id === "google") {
      return "auth-provider auth-provider--google";
    }
    return "auth-provider";
  }

  function providerIcon(providerId: string) {
    const id = providerId.toLowerCase();
    if (id === "github") {
      return <GithubMark />;
    }
    if (id === "google") {
      return <GoogleMark />;
    }
    return null;
  }

  useEffect(() => {
    fetch("/api/auth/providers")
      .then((res) => res.json())
      .then((providers) => {
        const list = Object.values(providers || {})
          .filter((provider: any) => provider.id !== "credentials")
          .map((provider: any) => ({ id: provider.id, name: provider.name }));
        setOauthProviders(list);
      })
      .catch(() => undefined);
  }, []);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setLoading(true);
    setError(null);

    const res = await fetch("/api/auth/register", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ name, email, password }),
    });
    const data = await res.json();
    if (!res.ok) {
      setError(data?.error || "Registration failed.");
      setLoading(false);
      return;
    }

    const login = await signIn("credentials", {
      redirect: false,
      email,
      password,
      callbackUrl: "/console",
    });
    if (login?.error) {
      setError("Account created, but login failed. Try signing in.");
      setLoading(false);
      return;
    }
    router.push("/console");
  }

  return (
    <section className="auth-shell">
      <div className="auth-panel">
        <header className="auth-header">
          <p className="auth-kicker">Create workspace access</p>
          <h2 className="auth-title">Create your account</h2>
          <p className="auth-subtitle">
            Set up billing and start using private context at scale.
          </p>
        </header>

        <form onSubmit={handleSubmit} className="auth-form">
          <label className="auth-field">
            <span className="auth-label">Name</span>
            <input
              className="auth-input"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label className="auth-field">
            <span className="auth-label">Email</span>
            <input
              className="auth-input"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              required
            />
          </label>
          <label className="auth-field">
            <span className="auth-label">Password</span>
            <input
              className="auth-input"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </label>

          {error ? <p className="auth-error">{error}</p> : null}

          <button type="submit" className="auth-submit" disabled={loading}>
            {loading ? "Creating..." : "Create account"}
          </button>
        </form>

        <div className="auth-divider">
          <span>or continue with</span>
        </div>
        <div className="auth-provider-grid">
          {(oauthProviders.length > 0 ? oauthProviders : fallbackProviders).map(
            (provider) => {
              const enabled = oauthProviders.length > 0;
              return (
                <button
                  key={provider.id}
                  className={providerClass(provider.id, enabled)}
                  onClick={
                    enabled
                      ? () => signIn(provider.id, { callbackUrl: "/console" })
                      : undefined
                  }
                  disabled={!enabled}
                  type="button"
                >
                  {providerIcon(provider.id)}
                  <span>Sign in with {provider.name}</span>
                </button>
              );
            },
          )}
        </div>

        {oauthProviders.length === 0 ? (
          <p className="auth-inline-warning">
            Social sign-in is not configured on this deployment yet.
          </p>
        ) : null}

        <p className="auth-meta">
          Already have an account?{" "}
          <a className="auth-link" href="/auth/login">
            Sign in
          </a>
          .
        </p>
      </div>
    </section>
  );
}
