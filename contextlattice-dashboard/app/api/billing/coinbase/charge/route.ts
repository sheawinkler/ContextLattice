import { PLANS } from "@/lib/billing/plans";
import { recordPaymentIntent } from "@/lib/billing/reconcile";
import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { fetchWithRetry } from "@/lib/http/retry";

export async function POST(request: Request) {
  const requestId = request.headers.get("x-request-id") || crypto.randomUUID();
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }
  const apiKey = process.env.COINBASE_COMMERCE_API_KEY;
  if (!apiKey) {
    return Response.json({ ok: false, error: "Missing COINBASE_COMMERCE_API_KEY" }, { status: 500 });
  }

  const { planId, interval } = await request.json();
  const plan = PLANS.find((p) => p.id === planId);
  if (!plan) {
    return Response.json({ ok: false, error: "Invalid plan" }, { status: 400 });
  }

  const billingInterval = interval === "annual" ? "annual" : "monthly";
  const amount = billingInterval === "annual" ? plan.annual : plan.monthly;
  const appUrl = process.env.APP_URL || "http://localhost:3000";

  const res = await fetchWithRetry("https://api.commerce.coinbase.com/charges", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-CC-Api-Key": apiKey,
      "X-CC-Version": "2018-03-22",
    },
    body: JSON.stringify({
      name: `ContextLattice ${plan.name}`,
      description: `ContextLattice ${billingInterval} plan`,
      pricing_type: "fixed_price",
      local_price: {
        amount: amount.toFixed(2),
        currency: "USD",
      },
      metadata: {
        planId: plan.id,
        interval: billingInterval,
        requestId,
      },
      redirect_url: `${appUrl}/billing?coinbase=success`,
      cancel_url: `${appUrl}/billing?coinbase=cancel`,
    }),
  });

  const data = await res.json();
  if (!res.ok) {
    return Response.json({ ok: false, error: data?.error?.message || "Coinbase error" }, { status: 400 });
  }

  await recordPaymentIntent({
    userId: session.user.id,
    provider: "coinbase",
    status: "created",
    planId: plan.id,
    interval: billingInterval,
    amount,
    currency: "USD",
    reference: data?.data?.id || "",
    metadata: JSON.stringify({ hosted_url: data?.data?.hosted_url }),
  });

  return Response.json({ ok: true, charge: data.data });
}
