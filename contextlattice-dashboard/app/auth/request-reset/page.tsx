"use client";

import { useState } from "react";

export default function RequestResetPage() {
  const [email, setEmail] = useState("");
  const [message, setMessage] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setLoading(true);
    setMessage(null);
    await fetch("/api/auth/request-reset", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ email }),
    });
    setMessage("If the email exists, a reset link has been sent.");
    setLoading(false);
  }

  return (
    <section className="auth-shell">
      <div className="auth-panel">
        <header className="auth-header">
          <p className="auth-kicker">Account recovery</p>
          <h2 className="auth-title">Reset your password</h2>
          <p className="auth-subtitle">
            We will email a reset link if your account exists.
          </p>
        </header>
        <form onSubmit={handleSubmit} className="auth-form">
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
          {message ? <p className="auth-inline-warning">{message}</p> : null}
          <button type="submit" className="auth-submit" disabled={loading}>
            {loading ? "Sending..." : "Send reset link"}
          </button>
        </form>
      </div>
    </section>
  );
}
