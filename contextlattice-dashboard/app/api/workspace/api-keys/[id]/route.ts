import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";
import { requireUserWorkspaceId } from "@/lib/workspace";
import { recordAuditLog } from "@/lib/audit";

export async function DELETE(
  _request: Request,
  context: { params: Promise<{ id: string }> },
) {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const { id } = await context.params;
  const workspaceId = await requireUserWorkspaceId(session.user.id);
  const key = await prisma.apiKey.findFirst({
    where: { id, workspaceId },
  });
  if (!key) {
    return Response.json({ ok: false, error: "Key not found" }, { status: 404 });
  }

  await prisma.apiKey.update({
    where: { id: key.id },
    data: { revokedAt: new Date() },
  });

  await recordAuditLog({
    workspaceId,
    userId: session.user.id,
    action: "api_key.revoke",
    targetType: "api_key",
    targetId: key.id,
    metadata: JSON.stringify({ name: key.name, prefix: key.prefix }),
  });

  return Response.json({ ok: true });
}
