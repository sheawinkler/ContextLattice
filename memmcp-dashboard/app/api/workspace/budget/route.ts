import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";
import { requireUserWorkspaceId } from "@/lib/workspace";
import { getUsageSummary } from "@/lib/usage/budgets";
import { recordAuditLog } from "@/lib/audit";

export async function GET() {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const workspaceId = await requireUserWorkspaceId(session.user.id);
  const budget = await prisma.usageBudget.findFirst({
    where: { workspaceId, active: true },
    orderBy: { createdAt: "desc" },
  });
  const usage = await getUsageSummary(workspaceId);

  return Response.json({ ok: true, budget, usage });
}

export async function POST(request: Request) {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const body = await request.json();
  const tokenLimit = body?.tokenLimit ? Number(body.tokenLimit) : null;
  const costLimitUsd = body?.costLimitUsd ? Number(body.costLimitUsd) : null;
  const workspaceId = await requireUserWorkspaceId(session.user.id);

  const budget = await prisma.usageBudget.create({
    data: {
      workspaceId,
      tokenLimit: Number.isFinite(tokenLimit) ? tokenLimit : null,
      costLimitUsd: Number.isFinite(costLimitUsd) ? costLimitUsd : null,
    },
  });

  await recordAuditLog({
    workspaceId,
    userId: session.user.id,
    action: "budget.create",
    targetType: "usage_budget",
    targetId: budget.id,
    metadata: JSON.stringify({
      tokenLimit: budget.tokenLimit,
      costLimitUsd: budget.costLimitUsd,
    }),
  });

  return Response.json({ ok: true, budget });
}
