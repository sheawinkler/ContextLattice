import { getDashboardSession } from "@/lib/dashboardSession";
import { prisma } from "@/lib/db";
import { generateApiKey } from "@/lib/auth/apiKeys";
import { requireUserWorkspaceId } from "@/lib/workspace";
import { recordAuditLog } from "@/lib/audit";
import { getWorkspacePlan, requireActiveSubscription } from "@/lib/billing/entitlements";
import { sendEmail } from "@/lib/email";

const DEFAULT_KEY_NAME = "Premium Runtime Key";
const DEFAULT_SCOPES =
  "memory:write,memory:search,usage:write,telemetry:read,status:read";

function roleAllowed(role: string | null | undefined): boolean {
  const normalized = String(role || "").trim().toLowerCase();
  return normalized === "owner" || normalized === "admin";
}

export async function POST(request: Request) {
  const session = await getDashboardSession();
  if (!session?.user?.id || !session.user.email) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }
  if (!roleAllowed(session.user.workspaceRole)) {
    return Response.json(
      { ok: false, error: "Only workspace owners/admins can issue premium keys." },
      { status: 403 },
    );
  }

  const body = await request.json().catch(() => ({} as Record<string, unknown>));
  const rotate = body.rotate === true;
  const name = String(body.name || DEFAULT_KEY_NAME).trim().slice(0, 120) || DEFAULT_KEY_NAME;
  const scopes = String(body.scopes || DEFAULT_SCOPES).trim() || DEFAULT_SCOPES;

  const workspaceId = await requireUserWorkspaceId(session.user.id);
  try {
    await requireActiveSubscription(workspaceId);
  } catch (err: any) {
    return Response.json(
      { ok: false, error: err?.message || "Active subscription required" },
      { status: 402 },
    );
  }

  const plan = await getWorkspacePlan(workspaceId);
  if (rotate) {
    await prisma.apiKey.updateMany({
      where: {
        workspaceId,
        revokedAt: null,
        name: { startsWith: DEFAULT_KEY_NAME },
      },
      data: { revokedAt: new Date() },
    });
  }

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
      scopes,
      createdByUserId: session.user.id,
    },
  });

  await recordAuditLog({
    workspaceId,
    userId: session.user.id,
    action: "billing.entitlement_key.issue",
    targetType: "api_key",
    targetId: record.id,
    metadata: JSON.stringify({
      planId: plan.planId,
      keyName: name,
      prefix,
      rotate,
    }),
  });

  const email = await sendEmail({
    to: session.user.email,
    subject: "ContextLattice premium runtime key",
    text: [
      `A premium runtime key was issued for your ${plan.planId} plan.`,
      `Name: ${name}`,
      `Prefix: ${prefix}`,
      "",
      `Key (shown once): ${apiKey}`,
      "",
      "Store this securely. It will not be shown again.",
    ].join("\n"),
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
    email,
  });
}
