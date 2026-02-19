import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";
import { requireUserWorkspaceId } from "@/lib/workspace";
import { generateScimToken } from "@/lib/auth/scim";
import { recordAuditLog } from "@/lib/audit";
import { getWorkspacePlan, requireActiveSubscription } from "@/lib/billing/entitlements";

export async function GET() {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const workspaceId = await requireUserWorkspaceId(session.user.id);
  const tokens = await prisma.scimToken.findMany({
    where: { workspaceId, revokedAt: null },
    orderBy: { createdAt: "desc" },
  });

  return Response.json({
    ok: true,
    tokens: tokens.map((token) => ({
      id: token.id,
      name: token.name,
      prefix: token.prefix,
      createdAt: token.createdAt,
      lastUsedAt: token.lastUsedAt,
    })),
  });
}

export async function POST(request: Request) {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const body = await request.json();
  const name = String(body?.name || "SCIM token").trim();
  const workspaceId = await requireUserWorkspaceId(session.user.id);
  try {
    await requireActiveSubscription(workspaceId);
  } catch (err: any) {
    return Response.json(
      { ok: false, error: err?.message || "Subscription required" },
      { status: 402 },
    );
  }
  const plan = await getWorkspacePlan(workspaceId);
  if (plan.planId !== "enterprise") {
    return Response.json(
      { ok: false, error: "SCIM is available on the Enterprise plan." },
      { status: 402 },
    );
  }
  const { token, prefix, tokenHash } = generateScimToken();

  const record = await prisma.scimToken.create({
    data: {
      workspaceId,
      name,
      prefix,
      tokenHash,
      createdByUserId: session.user.id,
    },
  });

  await recordAuditLog({
    workspaceId,
    userId: session.user.id,
    action: "scim_token.create",
    targetType: "scim_token",
    targetId: record.id,
    metadata: JSON.stringify({ name: record.name, prefix: record.prefix }),
  });

  return Response.json({
    ok: true,
    token: {
      id: record.id,
      name: record.name,
      prefix: record.prefix,
      token,
    },
  });
}
