"use client";

import { Suspense, useEffect, useState } from "react";
import { useSession } from "next-auth/react";
import { useSearchParams } from "next/navigation";
import { PLANS } from "@/lib/billing/plans";
import { dashboardClientAuthRequired } from "@/lib/authMode";

const intervals = ["monthly", "annual"] as const;

type Interval = (typeof intervals)[number];
type BillingSummary = {
  planId: string;
  entitlements: {
    maxApiKeys: number | null;
    maxProjects: number | null;
    maxWriteBytes: number | null;
  };
  subscription: {
    provider: string;
    status: string;
    planId?: string | null;
    currentPeriodEnd?: string | null;
  } | null;
  active: boolean;
  downloadEligible?: boolean;
  downloadConfigured?: boolean;
  requiresSubscription: boolean;
  intents: Array<{
    provider: string;
    status: string;
    planId: string;
    interval: string;
    amount: number;
    currency: string;
    createdAt: string;
  }>;
  failedIntentCount: number;
};

type DashboardSessionLike = {
  user?: {
    email?: string | null;
    workspaceRole?: string | null;
  } | null;
} | null;

function BillingPageContent({ session }: { session: DashboardSessionLike }) {
  const searchParams = useSearchParams();
  const [interval, setInterval] = useState<Interval>("monthly");
  const [message, setMessage] = useState<string | null>(null);
  const [solanaUrl, setSolanaUrl] = useState<string | null>(null);
  const [providers, setProviders] = useState<
    Record<string, { enabled: boolean; reason?: string }>
  >({});
  const [summary, setSummary] = useState<BillingSummary | null>(null);
  const [reconcileStatus, setReconcileStatus] = useState<{
    intents: Record<string, Record<string, number>>;
    failedWebhooks: number;
    windowDays: number;
  } | null>(null);
  const authRequired = dashboardClientAuthRequired();
  const accountBillingEnabled = authRequired && !!session?.user;

  useEffect(() => {
    if (searchParams.get("success") === "1") {
      setMessage(
        "Payment submitted. Once provider webhook finalizes, your premium download lane will unlock.",
      );
      return;
    }
    if (searchParams.get("canceled") === "1") {
      setMessage("Checkout canceled. You can retry any plan below.");
      return;
    }
    if (searchParams.get("paypal") === "success") {
      setMessage("PayPal authorization returned. Capture/webhook reconciliation is in progress.");
      return;
    }
    if (searchParams.get("paypal") === "cancel") {
      setMessage("PayPal checkout canceled.");
      return;
    }
    if (searchParams.get("coinbase") === "success") {
      setMessage("Coinbase payment submitted. Awaiting webhook confirmation.");
      return;
    }
    if (searchParams.get("coinbase") === "cancel") {
      setMessage("Coinbase checkout canceled.");
      return;
    }
  }, [searchParams]);

  useEffect(() => {
    let mounted = true;
    fetch("/api/billing/providers")
      .then((res) => res.json())
      .then((data) => {
        if (mounted && data?.providers) {
          setProviders(data.providers);
        }
      })
      .catch(() => undefined);
    return () => {
      mounted = false;
    };
  }, []);

  useEffect(() => {
    if (!accountBillingEnabled) {
      return;
    }
    fetch("/api/billing/reconcile/status")
      .then((res) => res.json())
      .then((data) => {
        if (data?.ok) {
          setReconcileStatus({
            intents: data.intents || {},
            failedWebhooks: data.failedWebhooks || 0,
            windowDays: data.windowDays || 30,
          });
        }
      })
      .catch(() => undefined);
  }, [accountBillingEnabled]);

  useEffect(() => {
    if (!accountBillingEnabled) {
      return;
    }
    fetch("/api/billing/summary")
      .then((res) => res.json())
      .then((data) => {
        if (data?.ok) {
          setSummary(data);
        }
      })
      .catch(() => undefined);
  }, [accountBillingEnabled]);

  async function startStripe(planId: string) {
    setMessage(null);
    const res = await fetch("/api/billing/stripe/checkout", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ planId, interval }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Stripe checkout failed");
      return;
    }
    window.location.href = data.url;
  }

  async function openStripePortal() {
    const res = await fetch("/api/billing/stripe/portal", { method: "POST" });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Portal error");
      return;
    }
    window.location.href = data.url;
  }

  async function startPayPal(planId: string) {
    setMessage(null);
    const res = await fetch("/api/billing/paypal/order", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ planId, interval }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "PayPal order failed");
      return;
    }
    const approve = data.links?.find((link: any) => link.rel === "approve");
    if (approve?.href) {
      window.location.href = approve.href;
    } else {
      setMessage("PayPal approval link missing.");
    }
  }

  async function startSolanaPay(planId: string) {
    setMessage(null);
    const res = await fetch("/api/billing/solana-pay", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ planId, interval }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Solana Pay error");
      return;
    }
    setSolanaUrl(data.url);
  }

  async function startKrak(planId: string) {
    setMessage(null);
    const res = await fetch("/api/billing/kraken", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ planId, interval }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Krak error");
      return;
    }
    setMessage(
      `Send ${data.amount} ${data.asset} to ${data.address}. ${data.instructions}`,
    );
  }

  async function startCoinbase(planId: string) {
    setMessage(null);
    const res = await fetch("/api/billing/coinbase/charge", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ planId, interval }),
    });
    const data = await res.json();
    if (!res.ok) {
      setMessage(data?.error || "Coinbase Commerce error");
      return;
    }
    if (data.charge?.hosted_url) {
      window.location.href = data.charge.hosted_url;
    } else {
      setMessage("Coinbase hosted URL missing.");
    }
  }

  return (
    <div className="space-y-6">
      <section className="card">
        <h2 className="text-xl font-semibold">Billing & Plans</h2>
        <p className="text-sm text-slate-400 mt-1">
          Choose a plan, then pick a payment method. We recommend card checkout
          first, PayPal next, and crypto if you prefer on-chain settlement.
        </p>
        <div className="mt-4 text-sm text-slate-300 space-y-1">
          <p><strong>Step 1:</strong> {authRequired ? "confirm you are signed in." : "run locally; dashboard accounts are off by default."}</p>
          <p><strong>Step 2:</strong> choose monthly or annual.</p>
          <p><strong>Step 3:</strong> complete checkout with any provider below.</p>
          <p><strong>Step 4:</strong> after confirmation, open <a className="underline" href="/downloads">Downloads</a>.</p>
        </div>
        {!authRequired ? (
          <p className="text-sm text-slate-300 mt-3">
            Local OSS mode is account-free. Billing actions stay inactive until you set <code>AUTH_REQUIRED=true</code>.
          </p>
        ) : !session?.user ? (
          <p className="text-sm text-amber-300 mt-3">
            You are not signed in. <a className="underline" href="/auth/login">Sign in</a> to attach billing to your account.
          </p>
        ) : (
          <p className="text-sm text-emerald-300 mt-3">
            Signed in as {session.user.email}
          </p>
        )}
        <div className="flex gap-2 mt-4">
          {intervals.map((value) => (
            <button
              key={value}
              className={`px-3 py-1 rounded border ${
                interval === value
                  ? "border-emerald-400 text-emerald-200"
                  : "border-slate-700 text-slate-400"
              }`}
              onClick={() => setInterval(value)}
            >
              {value === "monthly" ? "Monthly" : "Annual"}
            </button>
          ))}
        </div>
      </section>

      <section className="card space-y-2">
        <h3 className="text-lg font-semibold">Current plan</h3>
        {!authRequired ? (
          <p className="text-sm text-slate-400">
            Subscription summary is account-scoped and hidden in local OSS mode.
          </p>
        ) : !session?.user ? (
          <p className="text-sm text-slate-400">
            Sign in to view subscription summary and entitlements.
          </p>
        ) : summary ? (
          <>
            <p className="text-sm text-slate-300">
              Plan name:{" "}
              <span className="font-semibold">
                {PLANS.find((plan) => plan.id === summary.planId)?.name ||
                  summary.planId}
              </span>
            </p>
            <p className="text-sm text-slate-300">
              Plan: <span className="font-semibold">{summary.planId}</span>{" "}
              {summary.subscription?.status
                ? `(status: ${summary.subscription.status})`
                : "(no active subscription)"}
            </p>
            {summary.subscription?.currentPeriodEnd ? (
              <p className="text-xs text-slate-400">
                Renews on{" "}
                {new Date(summary.subscription.currentPeriodEnd).toLocaleDateString()}
              </p>
            ) : null}
            <p className="text-xs text-slate-400">
              API keys:{" "}
              {summary.entitlements.maxApiKeys === null
                ? "unlimited"
                : `up to ${summary.entitlements.maxApiKeys}`}
            </p>
            <p className="text-xs text-slate-400">
              Projects:{" "}
              {summary.entitlements.maxProjects === null
                ? "unlimited"
                : `up to ${summary.entitlements.maxProjects}`}
            </p>
            <p className="text-xs text-slate-400">
              Max write size:{" "}
              {summary.entitlements.maxWriteBytes === null
                ? "unlimited"
                : `${summary.entitlements.maxWriteBytes} bytes`}
            </p>
            {summary.requiresSubscription && !summary.active ? (
              <p className="text-xs text-amber-300">
                Subscription required to write memory or emit usage events.
              </p>
            ) : null}
            {summary.failedIntentCount > 0 ? (
              <p className="text-xs text-amber-300">
                {summary.failedIntentCount} recent payment attempts need attention.
              </p>
            ) : null}
            {summary.subscription &&
            ["past_due", "unpaid", "canceled"].includes(summary.subscription.status) ? (
              <p className="text-xs text-amber-300">
                Subscription is {summary.subscription.status}. Update payment
                details or switch providers to restore access.
              </p>
            ) : null}
            {summary.downloadConfigured && summary.downloadEligible ? (
              <div className="space-y-2">
                <p className="text-xs text-emerald-300">
                  Premium downloads are ready.
                </p>
                <a
                  className="inline-flex items-center rounded bg-emerald-500 text-emerald-950 px-3 py-1 text-xs font-semibold"
                  href="/downloads"
                >
                  Open Downloads
                </a>
              </div>
            ) : summary.downloadConfigured ? (
              <p className="text-xs text-amber-300">
                Premium artifacts are configured but locked until subscription becomes active.
              </p>
            ) : (
              <p className="text-xs text-amber-300">
                Premium artifacts are not configured by operator yet.
              </p>
            )}
          </>
        ) : (
          <p className="text-sm text-slate-400">Loading subscription summary…</p>
        )}
      </section>

      <section className="grid md:grid-cols-3 gap-4">
        {PLANS.map((plan) => (
          <div key={plan.id} className="card space-y-3">
            <div>
              <h3 className="text-lg font-semibold">{plan.name}</h3>
              <p className="text-sm text-slate-400">{plan.description}</p>
            </div>
            <div className="text-2xl font-semibold">
              ${interval === "monthly" ? plan.monthly : plan.annual}
              <span className="text-sm text-slate-400">
                /{interval === "monthly" ? "mo" : "yr"}
              </span>
            </div>
            <p className="text-sm text-slate-400">{plan.seats}</p>
            <ul className="text-sm text-slate-300 space-y-1">
              {plan.features.map((feature) => (
                <li key={feature}>• {feature}</li>
              ))}
            </ul>
            <div className="space-y-2">
              <button
                className="w-full rounded bg-emerald-500 text-emerald-950 py-2 font-semibold"
                onClick={() => startStripe(plan.id)}
                disabled={!accountBillingEnabled || (providers.stripe && !providers.stripe.enabled)}
              >
                Pay with card (Stripe Checkout)
              </button>
              {providers.stripe && !providers.stripe.enabled && providers.stripe.reason ? (
                <p className="text-xs text-amber-300">{providers.stripe.reason}</p>
              ) : null}
              <button
                className="w-full rounded border border-slate-700 py-2"
                onClick={() => startStripe(plan.id)}
                disabled={!accountBillingEnabled || (providers["apple-pay"] && !providers["apple-pay"].enabled)}
              >
                Pay with Apple Pay (via Stripe)
              </button>
              {providers["apple-pay"] &&
              !providers["apple-pay"].enabled &&
              providers["apple-pay"].reason ? (
                <p className="text-xs text-amber-300">{providers["apple-pay"].reason}</p>
              ) : null}
              <button
                className="w-full rounded border border-slate-700 py-2"
                onClick={() => startStripe(plan.id)}
                disabled={!accountBillingEnabled || (providers["google-pay"] && !providers["google-pay"].enabled)}
              >
                Pay with Google Pay (via Stripe)
              </button>
              {providers["google-pay"] &&
              !providers["google-pay"].enabled &&
              providers["google-pay"].reason ? (
                <p className="text-xs text-amber-300">{providers["google-pay"].reason}</p>
              ) : null}
              <button
                className="w-full rounded border border-slate-700 py-2"
                onClick={() => startPayPal(plan.id)}
                disabled={!accountBillingEnabled || (providers.paypal && !providers.paypal.enabled)}
              >
                Pay with PayPal
              </button>
              {providers.paypal && !providers.paypal.enabled && providers.paypal.reason ? (
                <p className="text-xs text-amber-300">{providers.paypal.reason}</p>
              ) : null}
              <button
                className="w-full rounded border border-slate-700 py-2"
                onClick={() => startSolanaPay(plan.id)}
                disabled={!accountBillingEnabled || (providers["solana-pay"] && !providers["solana-pay"].enabled)}
              >
                Pay with Solana Pay
              </button>
              {providers["solana-pay"] &&
              !providers["solana-pay"].enabled &&
              providers["solana-pay"].reason ? (
                <p className="text-xs text-amber-300">
                  {providers["solana-pay"].reason}
                </p>
              ) : null}
              <button
                className="w-full rounded border border-slate-700 py-2"
                onClick={() => startKrak(plan.id)}
                disabled={!accountBillingEnabled || (providers.kraken && !providers.kraken.enabled)}
              >
                Pay with Kraken
              </button>
              {providers.kraken && !providers.kraken.enabled && providers.kraken.reason ? (
                <p className="text-xs text-amber-300">{providers.kraken.reason}</p>
              ) : null}
              <button
                className="w-full rounded border border-slate-700 py-2"
                onClick={() => startCoinbase(plan.id)}
                disabled={!accountBillingEnabled || (providers.coinbase && !providers.coinbase.enabled)}
              >
                Pay with Coinbase Commerce
              </button>
              {providers.coinbase &&
              !providers.coinbase.enabled &&
              providers.coinbase.reason ? (
                <p className="text-xs text-amber-300">
                  {providers.coinbase.reason}
                </p>
              ) : null}
            </div>
          </div>
        ))}
      </section>

      <section className="card">
        <h3 className="text-lg font-semibold">Manage subscription</h3>
        <p className="text-sm text-slate-400">
          If you subscribed with Stripe, use the portal to update payment methods
          or cancel. Crypto subscriptions are reconciled manually until webhook verification is enabled.
        </p>
        <p className="text-xs text-slate-400 mt-2">
          By subscribing you agree to our{" "}
          <a className="underline" href="/legal/terms">
            Terms
          </a>
          ,{" "}
          <a className="underline" href="/legal/privacy">
            Privacy Policy
          </a>
          , and{" "}
          <a className="underline" href="/legal/refunds">
            Refund Policy
          </a>
          .
        </p>
        <button
          className="mt-4 rounded border border-slate-700 px-4 py-2"
          onClick={() => openStripePortal()}
          disabled={!accountBillingEnabled}
        >
          Open Stripe portal
        </button>
        <a
          className="mt-4 ml-2 inline-flex rounded border border-slate-700 px-4 py-2"
          href="/downloads"
        >
          Open premium downloads
        </a>
      </section>

      <section className="card">
        <h3 className="text-lg font-semibold">Recent payment activity</h3>
        {!accountBillingEnabled ? (
          <p className="text-sm text-slate-400 mt-2">Account billing history is disabled in local OSS mode.</p>
        ) : summary?.intents?.length ? (
          <div className="mt-3 space-y-2 text-sm text-slate-300">
            {summary.intents.map((intent, idx) => (
              <div key={`${intent.provider}-${intent.createdAt}-${idx}`} className="rounded border border-slate-800 px-3 py-2">
                <div className="flex items-center justify-between">
                  <div className="font-semibold capitalize">{intent.provider}</div>
                  <span className="text-xs text-slate-400">
                    {new Date(intent.createdAt).toLocaleString()}
                  </span>
                </div>
                <div className="text-xs text-slate-400 mt-1">
                  {intent.planId} • {intent.interval} • {intent.status}
                </div>
                <div className="text-xs text-slate-400">
                  {intent.amount} {intent.currency}
                </div>
              </div>
            ))}
          </div>
        ) : (
          <p className="text-sm text-slate-400 mt-2">No payment activity yet.</p>
        )}
      </section>

      <section className="card">
        <h3 className="text-lg font-semibold">Billing reconciliation</h3>
        <p className="text-sm text-slate-400">
          Recent billing intent status counts (last{" "}
          {reconcileStatus?.windowDays || 30} days).
        </p>
        {reconcileStatus ? (
          <div className="mt-3 space-y-2 text-sm text-slate-300">
            {Object.keys(reconcileStatus.intents).length === 0 ? (
              <p className="text-slate-400">No intents recorded yet.</p>
            ) : (
              Object.entries(reconcileStatus.intents).map(([provider, statuses]) => (
                <div key={provider} className="rounded border border-slate-800 px-3 py-2">
                  <div className="font-semibold capitalize">{provider}</div>
                  <div className="text-xs text-slate-400 mt-1">
                    {Object.entries(statuses)
                      .map(([status, count]) => `${status}: ${count}`)
                      .join(" • ")}
                  </div>
                </div>
              ))
            )}
            <p className="text-xs text-slate-400">
              Failed webhooks (last window): {reconcileStatus.failedWebhooks}
            </p>
            <p className="text-xs text-slate-500">
              Use{" "}
              <code>npm run billing:reconcile:stripe</code>,{" "}
              <code>npm run billing:reconcile:paypal</code>,{" "}
              <code>npm run billing:reconcile:coinbase</code> to sync provider
              status.
            </p>
          </div>
        ) : (
          <p className="text-sm text-slate-400 mt-2">Loading reconciliation status…</p>
        )}
      </section>

      {solanaUrl ? (
        <section className="card">
          <h3 className="text-lg font-semibold">Solana Pay link</h3>
          <p className="text-sm text-slate-400">
            Open this link in Phantom or another wallet.
          </p>
          <a className="text-emerald-300 break-all" href={solanaUrl}>
            {solanaUrl}
          </a>
        </section>
      ) : null}

      {message ? (
        <section className="card">
          <h3 className="text-lg font-semibold">Status</h3>
          <p className="text-sm text-slate-300">{message}</p>
          <p className="text-xs text-slate-400 mt-2">
            If payment just completed, refresh after webhook settlement and then continue to Downloads.
          </p>
        </section>
      ) : null}
    </div>
  );
}

function BillingPageWithSession() {
  const { data: session } = useSession();
  return <BillingPageContent session={session} />;
}

export default function BillingPage() {
  const content = dashboardClientAuthRequired() ? (
    <BillingPageWithSession />
  ) : (
    <BillingPageContent session={null} />
  );

  return (
    <Suspense fallback={<div className="card text-sm text-slate-400">Loading billing…</div>}>
      {content}
    </Suspense>
  );
}
