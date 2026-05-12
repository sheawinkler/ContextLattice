"use client";

import { Suspense, useState } from "react";
import { useSearchParams, useRouter } from "next/navigation";

function ResetPageContent() {
  const params = useSearchParams();
  const router = useRouter();
  const [password, setPassword] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const token = params.get("token") || "";
  const email = params.get("email") || "";

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setLoading(true);
    setMessage(null);
    const res = await fetch("/api/auth/reset", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ email, token, password }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Reset failed");
      setLoading(false);
      return;
    }
    setMessage("Password updated. You can sign in now.");
    setTimeout(() => router.push("/auth/login"), 1200);
  }

  return (
    <section className="auth-shell">
      <div className="auth-panel">
        <header className="auth-header">
          <p className="auth-kicker">Set new credentials</p>
          <h2 className="auth-title">Reset your password</h2>
        </header>
        <form onSubmit={handleSubmit} className="auth-form">
          <label className="auth-field">
            <span className="auth-label">New password</span>
            <input
              className="auth-input"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              required
            />
          </label>
          {message ? <p className="auth-inline-warning">{message}</p> : null}
          <button type="submit" className="auth-submit" disabled={loading}>
            {loading ? "Updating..." : "Reset password"}
          </button>
        </form>
      </div>
    </section>
  );
}

export default function ResetPage() {
  return (
    <Suspense
      fallback={
        <section className="auth-shell">
          <div className="auth-panel">Loading reset form...</div>
        </section>
      }
    >
      <ResetPageContent />
    </Suspense>
  );
}
