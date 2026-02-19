import { prisma } from "@/lib/db";

export type PlanEntitlements = {
  planId: string;
  maxApiKeys: number | null;
  maxProjects: number | null;
  maxWriteBytes: number | null;
};

const DEFAULT_PLAN_ID = process.env.DEFAULT_PLAN_ID || "starter";
const REQUIRE_ACTIVE_SUBSCRIPTION =
  process.env.REQUIRE_ACTIVE_SUBSCRIPTION === "true";

const PLAN_LIMITS: Record<string, PlanEntitlements> = {
  starter: {
    planId: "starter",
    maxApiKeys: 3,
    maxProjects: 5,
    maxWriteBytes: 50_000,
  },
  team: {
    planId: "team",
    maxApiKeys: 10,
    maxProjects: 25,
    maxWriteBytes: 200_000,
  },
  enterprise: {
    planId: "enterprise",
    maxApiKeys: null,
    maxProjects: null,
    maxWriteBytes: null,
  },
};

const ACTIVE_SUBSCRIPTION_STATUSES = new Set(["active", "trialing"]);

export function getEntitlements(planId?: string | null): PlanEntitlements {
  return PLAN_LIMITS[planId || DEFAULT_PLAN_ID] || PLAN_LIMITS[DEFAULT_PLAN_ID];
}

export function isSubscriptionActive(status?: string | null): boolean {
  if (!status) return false;
  return ACTIVE_SUBSCRIPTION_STATUSES.has(status);
}

export async function getWorkspacePlan(workspaceId: string) {
  const workspace = await prisma.workspace.findUnique({
    where: { id: workspaceId },
    select: { ownerId: true },
  });
  if (!workspace) {
    throw new Error("Workspace not found");
  }
  const subscription = await prisma.billingSubscription.findFirst({
    where: { userId: workspace.ownerId },
    orderBy: { updatedAt: "desc" },
  });
  const planId = subscription?.planId || DEFAULT_PLAN_ID;
  const entitlements = getEntitlements(planId);
  const active = isSubscriptionActive(subscription?.status);
  return { planId, entitlements, subscription, active };
}

export async function requireActiveSubscription(workspaceId: string) {
  if (!REQUIRE_ACTIVE_SUBSCRIPTION) {
    return { required: false };
  }
  const { active, planId, subscription } = await getWorkspacePlan(workspaceId);
  if (!active) {
    throw new Error(
      `Active subscription required (current: ${subscription?.status || "none"})`,
    );
  }
  return { required: true, planId, subscription };
}
