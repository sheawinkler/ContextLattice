import { PLANS } from "@/lib/billing/plans";
import { recordPaymentIntent } from "@/lib/billing/reconcile";
import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";

export async function POST(request: Request) {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }
  const { planId, interval } = await request.json();
  const plan = PLANS.find((p) => p.id === planId);
  if (!plan) {
    return Response.json({ ok: false, error: "Invalid plan" }, { status: 400 });
  }

  const billingInterval = interval === "annual" ? "annual" : "monthly";
  const amount = billingInterval === "annual" ? plan.annual : plan.monthly;

  const asset = process.env.KRAKEN_PAY_ASSET || "USDC";
  const address = process.env.KRAKEN_PAY_ADDRESS;
  const note = process.env.KRAKEN_PAY_NOTE || "ContextLattice subscription";

  if (!address) {
    return Response.json({ ok: false, error: "Missing KRAKEN_PAY_ADDRESS" }, { status: 500 });
  }

  await recordPaymentIntent({
    userId: session.user.id,
    provider: "kraken",
    status: "pending_manual",
    planId: plan.id,
    interval: billingInterval,
    amount,
    currency: "USD",
    reference: address,
    metadata: JSON.stringify({ asset, note }),
  });

  return Response.json({
    ok: true,
    provider: "kraken",
    asset,
    address,
    amount,
    note,
    instructions:
      "Send the requested amount to the Kraken deposit address. Payments are manually verified until Kraken Pay exposes a merchant API.",
  });
}
