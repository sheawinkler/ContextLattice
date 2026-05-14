import { headers } from "next/headers";
import { prisma } from "@/lib/db";
import { getStripeClient } from "@/lib/billing/stripe";
import { recordBillingEvent } from "@/lib/billing/events";
import { resolveStripePlanFromPriceId } from "@/lib/billing/providers";
import {
  upsertSubscriptionFromPaymentEvent,
  updatePaymentIntentStatus,
} from "@/lib/billing/reconcile";

export const runtime = "nodejs";

function configuredStripeWebhookSecrets(): string[] {
  const raw = [
    process.env.STRIPE_WEBHOOK_SECRET || "",
    process.env.STRIPE_WEBHOOK_THIN_SECRET || "",
    process.env.STRIPE_WEBHOOK_SNAPSHOT_SECRET || "",
    ...(process.env.STRIPE_WEBHOOK_SECRETS || "")
      .split(",")
      .map((value) => value.trim()),
  ];
  const unique = new Set<string>();
  for (const value of raw) {
    const trimmed = String(value || "").trim();
    if (trimmed) {
      unique.add(trimmed);
    }
  }
  return [...unique];
}

export async function POST(request: Request) {
  const requestId = request.headers.get("x-request-id") || crypto.randomUUID();
  const secrets = configuredStripeWebhookSecrets();
  if (!secrets.length) {
    return Response.json(
      {
        ok: false,
        error: "Stripe webhook not configured",
        code: "provider_not_configured",
      },
      { status: 503 },
    );
  }

  const sig = (await headers()).get("stripe-signature");
  const body = await request.text();

  if (!sig) {
    return Response.json({ ok: false, error: "Missing signature" }, { status: 400 });
  }

  const stripe = getStripeClient();
  let event;
  let verificationError: any = null;
  try {
    for (const secret of secrets) {
      try {
        event = stripe.webhooks.constructEvent(body, sig, secret);
        verificationError = null;
        break;
      } catch (err: any) {
        verificationError = err;
      }
    }
    if (!event) {
      throw verificationError || new Error("Webhook signature verification failed");
    }
  } catch (err: any) {
    return Response.json(
      {
        ok: false,
        error: err?.message || "Webhook signature verification failed",
        code: "signature_verification_failed",
      },
      { status: 400 },
    );
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
      if (session.id) {
        await updatePaymentIntentStatus("stripe", String(session.id), "completed");
      }
      const metadata = session.metadata || {};
      let planId = String(metadata.planId || "").trim() || null;
      let interval = String(metadata.interval || "").trim() || null;
      const metadataPriceId = String(metadata.priceId || "").trim();
      if ((!planId || !interval) && metadataPriceId) {
        const resolved = resolveStripePlanFromPriceId(metadataPriceId);
        if (resolved) {
          if (!planId) {
            planId = resolved.planId;
          }
          if (!interval) {
            interval = resolved.interval;
          }
        }
      }
      if (customerId && subscriptionId) {
        const customer = await prisma.billingCustomer.findFirst({
          where: { provider: "stripe", customerId },
        });
        if (customer) {
          let stripeSubStatus = "active";
          let currentPeriodEnd: Date | null = null;
          if (!planId || !interval) {
            const stripeSub = await stripe.subscriptions.retrieve(subscriptionId);
            stripeSubStatus = stripeSub.status || stripeSubStatus;
            currentPeriodEnd = stripeSub.current_period_end
              ? new Date(stripeSub.current_period_end * 1000)
              : null;
            const stripePriceId = stripeSub.items?.data?.[0]?.price?.id;
            const stripeMetadata = stripeSub.metadata || {};
            const resolved = resolveStripePlanFromPriceId(stripePriceId);
            if (!planId) {
              planId =
                String(stripeMetadata.planId || "").trim() ||
                resolved?.planId ||
                null;
            }
            if (!interval) {
              interval =
                String(stripeMetadata.interval || "").trim() ||
                resolved?.interval ||
                null;
            }
          }
          await prisma.billingSubscription.upsert({
            where: { provider_subscription: { provider: "stripe", subscription: subscriptionId } },
            update: {
              status: stripeSubStatus,
              planId,
              currentPeriodEnd,
            },
            create: {
              userId: customer.userId,
              provider: "stripe",
              subscription: subscriptionId,
              status: stripeSubStatus,
              planId,
              currentPeriodEnd,
            },
          });
          await upsertSubscriptionFromPaymentEvent({
            provider: "stripe",
            userId: customer.userId,
            status: stripeSubStatus,
            planId,
            interval,
            subscriptionRef: subscriptionId,
            currentPeriodEnd,
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
      const stripePriceId = subscription.items?.data?.[0]?.price?.id;
      const resolved = resolveStripePlanFromPriceId(stripePriceId);
      const metadata = subscription.metadata || {};
      const planId =
        String(metadata.planId || "").trim() ||
        resolved?.planId ||
        null;
      const interval =
        String(metadata.interval || "").trim() ||
        resolved?.interval ||
        null;
      await prisma.billingSubscription.updateMany({
        where: { provider: "stripe", subscription: subscriptionId },
        data: { status, currentPeriodEnd, planId },
      });
      let existing = await prisma.billingSubscription.findFirst({
        where: { provider: "stripe", subscription: subscriptionId },
      });
      if (!existing) {
        const customerId = String(subscription.customer || "").trim();
        if (customerId) {
          const customer = await prisma.billingCustomer.findFirst({
            where: { provider: "stripe", customerId },
          });
          if (customer) {
            existing = await prisma.billingSubscription.create({
              data: {
                userId: customer.userId,
                provider: "stripe",
                subscription: subscriptionId,
                status,
                planId,
                currentPeriodEnd,
              },
            });
          }
        }
      }
      if (existing) {
        await upsertSubscriptionFromPaymentEvent({
          provider: "stripe",
          userId: existing.userId,
          status,
          planId,
          interval,
          subscriptionRef: subscriptionId,
          currentPeriodEnd,
        });
      }
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
