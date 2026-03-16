"use client";

import { useState } from "react";
import { useSearchParams, useRouter } from "next/navigation";

export default function ResetPage() {
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
    <div className="max-w-md mx-auto mt-10 card">
      <h2 className="text-xl font-semibold">Reset your password</h2>
      <form onSubmit={handleSubmit} className="mt-6 space-y-4">
        <div className="space-y-1">
          <label className="text-sm text-slate-300">New password</label>
          <input
            className="w-full rounded border border-slate-700 bg-slate-900 px-3 py-2"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
          />
        </div>
        {message ? <p className="text-sm text-amber-300">{message}</p> : null}
        <button
          type="submit"
          className="w-full rounded bg-emerald-500 text-emerald-950 py-2 font-semibold"
          disabled={loading}
        >
          {loading ? "Updating..." : "Reset password"}
        </button>
      </form>
    </div>
  );
}
