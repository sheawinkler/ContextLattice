import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";
import { getStripeClient } from "@/lib/billing/stripe";
import { getStripePriceId } from "@/lib/billing/providers";
import { recordPaymentIntent } from "@/lib/billing/reconcile";

export async function POST(request: Request) {
  const session = await getServerSession(authOptions);
  if (!session?.user?.email || !session.user.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const body = await request.json();
  const planId = String(body?.planId || "");
  const interval = body?.interval === "annual" ? "annual" : "monthly";
  const priceId = getStripePriceId(planId, interval);
  if (!priceId) {
    return Response.json({ ok: false, error: "Invalid plan." }, { status: 400 });
  }

  const stripe = getStripeClient();
  const userId = session.user.id;

  const existing = await prisma.billingCustomer.findFirst({
    where: { userId, provider: "stripe" },
  });

  let customerId = existing?.customerId;
  if (!customerId) {
    const customer = await stripe.customers.create({
      email: session.user.email,
      name: session.user.name || undefined,
      metadata: { userId },
    });
    customerId = customer.id;
    await prisma.billingCustomer.create({
      data: {
        userId,
        provider: "stripe",
        customerId,
        email: session.user.email,
      },
    });
  }

  const appUrl = process.env.APP_URL || "http://localhost:3000";

  const checkout = await stripe.checkout.sessions.create({
    mode: "subscription",
    customer: customerId,
    line_items: [{ price: priceId, quantity: 1 }],
    success_url: `${appUrl}/billing?success=1`,
    cancel_url: `${appUrl}/billing?canceled=1`,
    allow_promotion_codes: true,
    client_reference_id: userId,
  });

  await recordPaymentIntent({
    userId,
    provider: "stripe",
    status: "created",
    planId,
    interval,
    amount: 0,
    currency: "USD",
    reference: checkout.id,
    metadata: JSON.stringify({ checkoutUrl: checkout.url }),
  });

  return Response.json({ ok: true, url: checkout.url });
}
