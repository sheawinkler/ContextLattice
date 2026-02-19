import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";
import { recordAuditLog } from "@/lib/audit";

export async function POST() {
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
      { ok: false, error: "Only workspace owners can request deletion." },
      { status: 403 },
    );
  }

  const workspaceId = membership.workspaceId;
  await prisma.workspace.update({
    where: { id: workspaceId },
    data: {
      status: "pending_delete",
      deletionRequestedAt: new Date(),
    },
  });
  await prisma.apiKey.updateMany({
    where: { workspaceId, revokedAt: null },
    data: { revokedAt: new Date() },
  });
  await prisma.usageBudget.updateMany({
    where: { workspaceId, active: true },
    data: { active: false },
  });

  await recordAuditLog({
    workspaceId,
    userId: session.user.id,
    action: "workspace.delete.request",
    targetType: "workspace",
    targetId: workspaceId,
  });

  return Response.json({
    ok: true,
    status: "pending_delete",
    note:
      "Workspace marked for deletion. Memory bank data must be purged separately from the memorymcp store.",
  });
}
