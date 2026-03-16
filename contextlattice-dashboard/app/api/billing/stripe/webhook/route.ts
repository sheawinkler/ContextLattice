import { headers } from "next/headers";
import { prisma } from "@/lib/db";
import { getStripeClient } from "@/lib/billing/stripe";
import { recordBillingEvent } from "@/lib/billing/events";

export const runtime = "nodejs";

export async function POST(request: Request) {
  const requestId = request.headers.get("x-request-id") || crypto.randomUUID();
  const secret = process.env.STRIPE_WEBHOOK_SECRET;
  if (!secret) {
    return Response.json({ ok: false, error: "Missing webhook secret" }, { status: 500 });
  }

  const sig = headers().get("stripe-signature");
  const body = await request.text();

  if (!sig) {
    return Response.json({ ok: false, error: "Missing signature" }, { status: 400 });
  }

  const stripe = getStripeClient();
  let event;
  try {
    event = stripe.webhooks.constructEvent(body, sig, secret);
  } catch (err: any) {
    return Response.json({ ok: false, error: err.message }, { status: 400 });
  }

  await recordBillingEvent({
    provider: "stripe",
    eventId: event.id,
    eventType: event.type,
    payload: JSON.stringify(event),
    status: "received",
    requestId,
  });

  try {
    if (event.type === "checkout.session.completed") {
      const session = event.data.object as any;
      const customerId = session.customer as string | null;
      const subscriptionId = session.subscription as string | null;
      if (customerId && subscriptionId) {
        const customer = await prisma.billingCustomer.findFirst({
          where: { provider: "stripe", customerId },
        });
        if (customer) {
          await prisma.billingSubscription.upsert({
            where: { provider_subscription: { provider: "stripe", subscription: subscriptionId } },
            update: { status: "active" },
            create: {
              userId: customer.userId,
              provider: "stripe",
              subscription: subscriptionId,
              status: "active",
            },
          });
        }
      }
    }

    if (
      event.type === "customer.subscription.updated" ||
      event.type === "customer.subscription.deleted"
    ) {
      const subscription = event.data.object as any;
      const subscriptionId = subscription.id as string;
      const status = subscription.status as string;
      const currentPeriodEnd = subscription.current_period_end
        ? new Date(subscription.current_period_end * 1000)
        : null;
      await prisma.billingSubscription.updateMany({
        where: { provider: "stripe", subscription: subscriptionId },
        data: { status, currentPeriodEnd },
      });
    }

    await recordBillingEvent({
      provider: "stripe",
      eventId: event.id,
      eventType: event.type,
      payload: JSON.stringify(event),
      status: "processed",
      requestId,
    });
  } catch (err: any) {
    await recordBillingEvent({
      provider: "stripe",
      eventId: event.id,
      eventType: event.type,
      payload: JSON.stringify(event),
      status: "failed",
      error: err?.message || "stripe webhook error",
      requestId,
    });
    return Response.json({ ok: false, error: "Webhook processing failed" }, { status: 500 });
  }

  return Response.json({ ok: true });
}
