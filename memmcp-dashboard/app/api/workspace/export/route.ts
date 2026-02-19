import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";
import { recordAuditLog } from "@/lib/audit";

export async function GET() {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const membership = await prisma.workspaceMember.findFirst({
    where: { userId: session.user.id },
    include: { workspace: true },
  });
  if (!membership) {
    return Response.json({ ok: false, error: "Workspace not found" }, { status: 404 });
  }
  if (membership.role !== "owner") {
    return Response.json(
      { ok: false, error: "Only workspace owners can export data." },
      { status: 403 },
    );
  }

  const workspaceId = membership.workspaceId;
  await recordAuditLog({
    workspaceId,
    userId: session.user.id,
    action: "workspace.export",
    targetType: "workspace",
    targetId: workspaceId,
  });
  const [members, apiKeys, budgets, usageEvents, auditLogs, paymentIntents] =
    await Promise.all([
      prisma.workspaceMember.findMany({ where: { workspaceId } }),
      prisma.apiKey.findMany({
        where: { workspaceId },
        select: {
          id: true,
          name: true,
          prefix: true,
          createdAt: true,
          lastUsedAt: true,
          revokedAt: true,
        },
      }),
      prisma.usageBudget.findMany({ where: { workspaceId } }),
      prisma.usageEvent.findMany({
        where: { workspaceId },
        orderBy: { createdAt: "desc" },
        take: 5000,
      }),
      prisma.auditLog.findMany({
        where: { workspaceId },
        orderBy: { createdAt: "desc" },
        take: 5000,
      }),
      prisma.paymentIntent.findMany({
        where: { userId: session.user.id },
        orderBy: { createdAt: "desc" },
        take: 2000,
      }),
    ]);

  return Response.json({
    ok: true,
    exportedAt: new Date().toISOString(),
    workspace: membership.workspace,
    members,
    apiKeys,
    budgets,
    usageEvents,
    auditLogs,
    paymentIntents,
    note:
      "Memory bank files are stored in the memorymcp service and must be exported separately.",
  });
}
