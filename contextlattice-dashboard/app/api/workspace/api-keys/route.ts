import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { prisma } from "@/lib/db";
import { generateApiKey } from "@/lib/auth/apiKeys";
import { requireUserWorkspaceId } from "@/lib/workspace";
import { recordAuditLog } from "@/lib/audit";
import { getWorkspacePlan, requireActiveSubscription } from "@/lib/billing/entitlements";

export async function GET() {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const workspaceId = await requireUserWorkspaceId(session.user.id);
  const keys = await prisma.apiKey.findMany({
    where: { workspaceId, revokedAt: null },
    orderBy: { createdAt: "desc" },
  });
  const plan = await getWorkspacePlan(workspaceId);

  return Response.json({
    ok: true,
    keys: keys.map((key) => ({
      id: key.id,
      name: key.name,
      prefix: key.prefix,
      scopes: key.scopes,
      createdAt: key.createdAt,
      lastUsedAt: key.lastUsedAt,
    })),
    planId: plan.planId,
    limit: plan.entitlements.maxApiKeys,
  });
}

export async function POST(request: Request) {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const body = await request.json();
  const name = String(body?.name || "Default key").trim();
  const scopesInput = Array.isArray(body?.scopes)
    ? body.scopes.join(",")
    : String(body?.scopes || "memory:write,usage:write");
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
  if (plan.entitlements.maxApiKeys !== null) {
    const existingCount = await prisma.apiKey.count({
      where: { workspaceId, revokedAt: null },
    });
    if (existingCount >= plan.entitlements.maxApiKeys) {
      return Response.json(
        {
          ok: false,
          error: `API key limit reached for ${plan.planId} plan.`,
        },
        { status: 402 },
      );
    }
  }
  const { apiKey, prefix, keyHash } = generateApiKey();

  const record = await prisma.apiKey.create({
    data: {
      workspaceId,
      name,
      prefix,
      keyHash,
      scopes: scopesInput,
      createdByUserId: session.user.id,
    },
  });

  await recordAuditLog({
    workspaceId,
    userId: session.user.id,
    action: "api_key.create",
    targetType: "api_key",
    targetId: record.id,
    metadata: JSON.stringify({ name: record.name, prefix: record.prefix }),
  });

  return Response.json({
    ok: true,
    key: {
      id: record.id,
      name: record.name,
      prefix: record.prefix,
      scopes: record.scopes,
      apiKey,
    },
  });
}
