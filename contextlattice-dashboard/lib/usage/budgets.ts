import { prisma } from "@/lib/db";

function startOfCurrentMonth() {
  const now = new Date();
  return new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1, 0, 0, 0));
}

export async function getActiveBudget(workspaceId: string) {
  return prisma.usageBudget.findFirst({
    where: { workspaceId, active: true },
    orderBy: { createdAt: "desc" },
  });
}

export async function getUsageSummary(workspaceId: string) {
  const periodStart = startOfCurrentMonth();
  const aggregate = await prisma.usageEvent.aggregate({
    where: { workspaceId, createdAt: { gte: periodStart } },
    _sum: { tokens: true, costUsd: true },
  });
  return {
    periodStart,
    tokens: aggregate._sum.tokens ?? 0,
    costUsd: aggregate._sum.costUsd ?? 0,
  };
}

export async function checkUsageLimits(
  workspaceId: string,
  delta?: { tokens?: number | null; costUsd?: number | null },
) {
  const budget = await getActiveBudget(workspaceId);
  if (!budget) {
    return { ok: true, budget: null, summary: null } as const;
  }
  const summary = await getUsageSummary(workspaceId);
  const tokensDelta = Number.isFinite(delta?.tokens) ? Number(delta?.tokens) : 0;
  const costDelta = Number.isFinite(delta?.costUsd) ? Number(delta?.costUsd) : 0;
  const projectedTokens = summary.tokens + tokensDelta;
  const projectedCost = summary.costUsd + costDelta;
  if (budget.tokenLimit && projectedTokens > budget.tokenLimit) {
    return {
      ok: false,
      budget,
      summary,
      reason: "Token budget exceeded",
    } as const;
  }
  if (budget.costLimitUsd && projectedCost > budget.costLimitUsd) {
    return {
      ok: false,
      budget,
      summary,
      reason: "Cost budget exceeded",
    } as const;
  }
  return { ok: true, budget, summary } as const;
}
