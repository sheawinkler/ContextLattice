"use client";

import { useEffect, useState } from "react";
import { signIn } from "next-auth/react";
import { useRouter, useSearchParams } from "next/navigation";

export default function LoginPage() {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [oauthProviders, setOauthProviders] = useState<
    Array<{ id: string; name: string }>
  >([]);
  const router = useRouter();
  const params = useSearchParams();
  const callbackUrl = params.get("callbackUrl") || "/billing";

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

  return (
    <div className="max-w-md mx-auto mt-10 card">
      <h2 className="text-xl font-semibold">Sign in</h2>
      <p className="text-sm text-slate-400 mt-1">
        Access billing, usage, and your private context workspace.
      </p>
      <form onSubmit={handleSubmit} className="mt-6 space-y-4">
        <div className="space-y-1">
          <label className="text-sm text-slate-300">Email</label>
          <input
            className="w-full rounded border border-slate-700 bg-slate-900 px-3 py-2"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            required
          />
        </div>
        <div className="space-y-1">
          <label className="text-sm text-slate-300">Password</label>
          <input
            className="w-full rounded border border-slate-700 bg-slate-900 px-3 py-2"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
          />
        </div>
        {error ? <p className="text-sm text-rose-300">{error}</p> : null}
        <button
          type="submit"
          className="w-full rounded bg-emerald-500 text-emerald-950 py-2 font-semibold"
          disabled={loading}
        >
          {loading ? "Signing in..." : "Sign in"}
        </button>
      </form>
      {oauthProviders.length > 0 ? (
        <div className="mt-6 space-y-2">
          <div className="text-xs uppercase tracking-wide text-slate-500">
            Or continue with
          </div>
          {oauthProviders.map((provider) => (
            <button
              key={provider.id}
              className="w-full rounded border border-slate-700 py-2 text-sm"
              onClick={() => signIn(provider.id, { callbackUrl })}
            >
              Sign in with {provider.name}
            </button>
          ))}
        </div>
      ) : null}
      <p className="text-sm text-slate-400 mt-4">
        Need an account?{" "}
        <a className="text-emerald-300" href="/auth/register">
          Create one
        </a>
        .
      </p>
      <p className="text-sm text-slate-400 mt-2">
        Forgot password?{" "}
        <a className="text-emerald-300" href="/auth/request-reset">
          Reset it
        </a>
        .
      </p>
    </div>
  );
}
