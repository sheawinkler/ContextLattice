import { prisma } from "@/lib/db";

const ACTIVE_PAYMENT_STATUSES = new Set([
  "active",
  "captured",
  "completed",
  "confirmed",
  "paid",
  "resolved",
  "succeeded",
  "trialing",
]);

const CANCELED_PAYMENT_STATUSES = new Set([
  "canceled",
  "cancelled",
  "chargeback",
  "denied",
  "expired",
  "failed",
  "refunded",
  "reversed",
  "unpaid",
]);

export async function recordPaymentIntent({
  userId,
  provider,
  status,
  planId,
  interval,
  amount,
  currency,
  reference,
  metadata,
}: {
  userId: string;
  provider: string;
  status: string;
  planId: string;
  interval: string;
  amount: number;
  currency: string;
  reference?: string;
  metadata?: string;
}) {
  return prisma.paymentIntent.create({
    data: {
      userId,
      provider,
      status,
      planId,
      interval,
      amount,
      currency,
      reference,
      metadata,
    },
  });
}

function normalizeStatus(input: string): string {
  return String(input || "").trim().toLowerCase();
}

function mapPaymentStatusToSubscriptionStatus(provider: string, status: string): string {
  const normalizedProvider = normalizeStatus(provider);
  const normalizedStatus = normalizeStatus(status);
  if (normalizedProvider === "stripe") {
    if (ACTIVE_PAYMENT_STATUSES.has(normalizedStatus)) {
      return normalizedStatus === "trialing" ? "trialing" : "active";
    }
    if (CANCELED_PAYMENT_STATUSES.has(normalizedStatus)) {
      return "canceled";
    }
    return "pending";
  }
  if (ACTIVE_PAYMENT_STATUSES.has(normalizedStatus)) {
    return "active";
  }
  if (CANCELED_PAYMENT_STATUSES.has(normalizedStatus)) {
    return "canceled";
  }
  return "pending";
}

function resolveCurrentPeriodEnd(
  interval: string | null | undefined,
  fromDate: Date,
): Date | null {
  const normalized = normalizeStatus(interval || "");
  const anchor = new Date(fromDate);
  if (normalized === "annual" || normalized === "yearly") {
    anchor.setUTCFullYear(anchor.getUTCFullYear() + 1);
    return anchor;
  }
  if (normalized === "monthly") {
    anchor.setUTCMonth(anchor.getUTCMonth() + 1);
    return anchor;
  }
  return null;
}

type UpsertSubscriptionInput = {
  provider: string;
  userId: string;
  status: string;
  planId?: string | null;
  interval?: string | null;
  subscriptionRef?: string | null;
  currentPeriodEnd?: Date | null;
};

export async function upsertSubscriptionFromPaymentEvent(
  input: UpsertSubscriptionInput,
) {
  const provider = normalizeStatus(input.provider);
  const mappedStatus = mapPaymentStatusToSubscriptionStatus(provider, input.status);
  const planId = (input.planId || "").trim() || null;
  const interval = (input.interval || "").trim();
  const subscriptionRef =
    (input.subscriptionRef || "").trim() || `${provider}:${input.userId}`;
  const now = new Date();
  const currentPeriodEnd =
    input.currentPeriodEnd ??
    (mappedStatus === "active" || mappedStatus === "trialing"
      ? resolveCurrentPeriodEnd(interval, now)
      : null);

  return prisma.billingSubscription.upsert({
    where: {
      provider_subscription: {
        provider,
        subscription: subscriptionRef,
      },
    },
    update: {
      userId: input.userId,
      status: mappedStatus,
      planId,
      currentPeriodEnd,
    },
    create: {
      userId: input.userId,
      provider,
      subscription: subscriptionRef,
      status: mappedStatus,
      planId,
      currentPeriodEnd,
    },
  });
}

export async function updatePaymentIntentStatus(
  provider: string,
  reference: string,
  status: string,
) {
  const normalizedProvider = normalizeStatus(provider);
  await prisma.paymentIntent.updateMany({
    where: { provider: normalizedProvider, reference },
    data: { status },
  });

  if (normalizedProvider === "stripe") {
    // Stripe subscription state is managed via webhook subscription events.
    return;
  }

  const intents = await prisma.paymentIntent.findMany({
    where: { provider: normalizedProvider, reference },
    orderBy: { updatedAt: "desc" },
  });

  for (const intent of intents) {
    await upsertSubscriptionFromPaymentEvent({
      provider: normalizedProvider,
      userId: intent.userId,
      status,
      planId: intent.planId,
      interval: intent.interval,
      subscriptionRef: `${normalizedProvider}:${intent.userId}`,
      currentPeriodEnd:
        mapPaymentStatusToSubscriptionStatus(normalizedProvider, status) === "active"
          ? resolveCurrentPeriodEnd(intent.interval, intent.updatedAt || new Date())
          : null,
    });
  }
}
