import { prisma } from "@/lib/db";
import { getStripeClient } from "@/lib/billing/stripe";
import { resolveStripePlanFromPriceId } from "@/lib/billing/providers";
import { upsertSubscriptionFromPaymentEvent } from "@/lib/billing/reconcile";

const applyChanges = process.env.BILLING_RECONCILE_APPLY === "true";
const lookbackDays = Number(process.env.BILLING_RECONCILE_LOOKBACK_DAYS || "7");

async function reconcileCheckoutSessions() {
  const stripe = getStripeClient();
  const cutoff = Math.floor(Date.now() / 1000) - lookbackDays * 24 * 60 * 60;

  const intents = await prisma.paymentIntent.findMany({
    where: { provider: "stripe" },
    orderBy: { createdAt: "desc" },
    take: 500,
  });

  let updated = 0;
  for (const intent of intents) {
    if (!intent.reference) continue;
    const session = await stripe.checkout.sessions.retrieve(intent.reference);
    const nextStatus =
      session.payment_status === "paid"
        ? "paid"
        : session.status === "complete"
          ? "complete"
          : session.payment_status || session.status || intent.status;

    if (nextStatus !== intent.status) {
      console.log(
        `[stripe] intent ${intent.reference} ${intent.status} -> ${nextStatus}`,
      );
      if (applyChanges) {
        await prisma.paymentIntent.update({
          where: { id: intent.id },
          data: { status: nextStatus },
        });
      }
      updated += 1;
    }
  }
  console.log(`[stripe] intents reconciled: ${updated} updated`);

  const customers = await prisma.billingCustomer.findMany({
    where: { provider: "stripe" },
  });

  let subsUpdated = 0;
  for (const customer of customers) {
    const subs = await stripe.subscriptions.list({
      customer: customer.customerId,
      status: "all",
      limit: 100,
      created: { gte: cutoff },
    });
    for (const sub of subs.data) {
      const status = sub.status;
      const currentPeriodEnd = sub.current_period_end
        ? new Date(sub.current_period_end * 1000)
        : null;
      const stripePriceId = sub.items?.data?.[0]?.price?.id;
      const resolved = resolveStripePlanFromPriceId(stripePriceId);
      const metadata = sub.metadata || {};
      const planId =
        String(metadata.planId || "").trim() ||
        resolved?.planId ||
        null;
      const interval =
        String(metadata.interval || "").trim() ||
        resolved?.interval ||
        null;
      const existing = await prisma.billingSubscription.findFirst({
        where: { provider: "stripe", subscription: sub.id },
      });
      if (!existing) {
        console.log(`[stripe] new subscription ${sub.id} status=${status}`);
        if (applyChanges) {
          await prisma.billingSubscription.create({
            data: {
              userId: customer.userId,
              provider: "stripe",
              subscription: sub.id,
              status,
              planId,
              currentPeriodEnd,
            },
          });
        }
        if (applyChanges) {
          await upsertSubscriptionFromPaymentEvent({
            provider: "stripe",
            userId: customer.userId,
            status,
            planId,
            interval,
            subscriptionRef: sub.id,
            currentPeriodEnd,
          });
        }
        subsUpdated += 1;
        continue;
      }
      if (
        existing.status !== status ||
        (existing.planId || null) !== (planId || null) ||
        existing.currentPeriodEnd?.getTime() !== currentPeriodEnd?.getTime()
      ) {
        console.log(`[stripe] subscription ${sub.id} status=${existing.status} -> ${status}`);
        if (applyChanges) {
          await prisma.billingSubscription.update({
            where: { id: existing.id },
            data: { status, planId, currentPeriodEnd },
          });
          await upsertSubscriptionFromPaymentEvent({
            provider: "stripe",
            userId: existing.userId,
            status,
            planId,
            interval,
            subscriptionRef: sub.id,
            currentPeriodEnd,
          });
        }
        subsUpdated += 1;
      }
    }
  }
  console.log(`[stripe] subscriptions reconciled: ${subsUpdated} updated`);
}

reconcileCheckoutSessions()
  .catch((err) => {
    console.error("[stripe] reconcile error", err);
    process.exitCode = 1;
  })
  .finally(async () => {
    await prisma.$disconnect();
  });
