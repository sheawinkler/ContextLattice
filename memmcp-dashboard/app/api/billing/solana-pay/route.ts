import { encodeURL } from "@solana/pay";
import { Keypair, PublicKey } from "@solana/web3.js";
import BigNumber from "bignumber.js";
import { PLANS } from "@/lib/billing/plans";
import { recordPaymentIntent } from "@/lib/billing/reconcile";
import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";

export async function POST(request: Request) {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }
  if (process.env.SOLANA_PAY_ENABLED === "false") {
    return Response.json({ ok: false, error: "Solana Pay is disabled." }, { status: 503 });
  }
  const { planId, interval } = await request.json();
  const plan = PLANS.find((p) => p.id === planId);
  if (!plan) {
    return Response.json({ ok: false, error: "Invalid plan" }, { status: 400 });
  }

  const recipient = process.env.SOLPAY_RECIPIENT;
  if (!recipient) {
    return Response.json({ ok: false, error: "Missing SOLPAY_RECIPIENT" }, { status: 500 });
  }

  const billingInterval = interval === "annual" ? "annual" : "monthly";
  const amount = billingInterval === "annual" ? plan.annual : plan.monthly;
  const reference = Keypair.generate().publicKey;
  const splToken = process.env.SOLPAY_SPL_TOKEN;

  const url = encodeURL({
    recipient: new PublicKey(recipient),
    amount: new BigNumber(amount),
    splToken: splToken ? new PublicKey(splToken) : undefined,
    reference,
    label: process.env.SOLPAY_LABEL || "ContextLattice",
    message: process.env.SOLPAY_MESSAGE || `ContextLattice ${plan.name} (${billingInterval})`,
    memo: process.env.SOLPAY_MEMO || `contextlattice:${plan.id}:${billingInterval}`,
  });

  await recordPaymentIntent({
    userId: session.user.id,
    provider: "solana-pay",
    status: "created",
    planId: plan.id,
    interval: billingInterval,
    amount,
    currency: "USD",
    reference: reference.toBase58(),
    metadata: JSON.stringify({ solpayUrl: url.toString() }),
  });

  return Response.json({
    ok: true,
    reference: reference.toBase58(),
    url: url.toString(),
  });
}
