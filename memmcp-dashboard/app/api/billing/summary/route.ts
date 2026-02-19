import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";
import { getEntitlements, isSubscriptionActive } from "@/lib/billing/entitlements";

const DEFAULT_PLAN_ID = process.env.DEFAULT_PLAN_ID || "starter";
const REQUIRE_ACTIVE_SUBSCRIPTION =
  process.env.REQUIRE_ACTIVE_SUBSCRIPTION === "true";

export async function GET() {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const subscription = await prisma.billingSubscription.findFirst({
    where: { userId: session.user.id },
    orderBy: { updatedAt: "desc" },
  });
  const planId = subscription?.planId || DEFAULT_PLAN_ID;
  const entitlements = getEntitlements(planId);
  const active = isSubscriptionActive(subscription?.status);

  const intents = await prisma.paymentIntent.findMany({
    where: { userId: session.user.id },
    orderBy: { createdAt: "desc" },
    take: 10,
  });

  const failedIntents = intents.filter((intent) =>
    ["failed", "canceled", "requires_action"].includes(intent.status),
  );

  return Response.json({
    ok: true,
    planId,
    entitlements,
    subscription: subscription
      ? {
          provider: subscription.provider,
          status: subscription.status,
          planId: subscription.planId,
          currentPeriodEnd: subscription.currentPeriodEnd,
        }
      : null,
    active,
    requiresSubscription: REQUIRE_ACTIVE_SUBSCRIPTION,
    intents: intents.map((intent) => ({
      provider: intent.provider,
      status: intent.status,
      planId: intent.planId,
      interval: intent.interval,
      amount: intent.amount,
      currency: intent.currency,
      createdAt: intent.createdAt,
    })),
    failedIntentCount: failedIntents.length,
  });
}
