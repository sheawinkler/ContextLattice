import { prisma } from "@/lib/db";

export async function recordAuditLog(input: {
  workspaceId: string;
  userId?: string | null;
  action: string;
  targetType?: string | null;
  targetId?: string | null;
  metadata?: string | null;
}) {
  return prisma.auditLog.create({
    data: {
      workspaceId: input.workspaceId,
      userId: input.userId ?? null,
      action: input.action,
      targetType: input.targetType ?? null,
      targetId: input.targetId ?? null,
      metadata: input.metadata ?? null,
    },
  });
}

export async function listAuditLogs(workspaceId: string, limit = 50) {
  return prisma.auditLog.findMany({
    where: { workspaceId },
    orderBy: { createdAt: "desc" },
    take: limit,
  });
}
