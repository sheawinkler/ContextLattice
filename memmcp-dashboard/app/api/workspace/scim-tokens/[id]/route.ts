import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";
import { requireUserWorkspaceId } from "@/lib/workspace";
import { recordAuditLog } from "@/lib/audit";

export async function DELETE(
  _request: Request,
  context: { params: { id: string } },
) {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const workspaceId = await requireUserWorkspaceId(session.user.id);
  const token = await prisma.scimToken.findFirst({
    where: { id: context.params.id, workspaceId },
  });
  if (!token) {
    return Response.json({ ok: false, error: "Token not found" }, { status: 404 });
  }

  await prisma.scimToken.update({
    where: { id: token.id },
    data: { revokedAt: new Date() },
  });

  await recordAuditLog({
    workspaceId,
    userId: session.user.id,
    action: "scim_token.revoke",
    targetType: "scim_token",
    targetId: token.id,
    metadata: JSON.stringify({ name: token.name, prefix: token.prefix }),
  });

  return Response.json({ ok: true });
}
