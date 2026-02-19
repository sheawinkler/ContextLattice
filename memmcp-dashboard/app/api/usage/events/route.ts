import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { recordUsageEvent } from "@/lib/usage/events";
import { checkUsageLimits, getUsageSummary } from "@/lib/usage/budgets";
import { extractApiKey, authenticateApiKey, hasScope } from "@/lib/auth/apiKeys";
import { ensureWorkspaceActive, requireActiveWorkspaceId } from "@/lib/workspace";
import { recordAuditLog } from "@/lib/audit";
import { requireActiveSubscription } from "@/lib/billing/entitlements";

export async function POST(request: Request) {
  const rawKey = extractApiKey(request);
  const session = await getServerSession(authOptions);

  let workspaceId: string | null = null;
  let userId: string | null = null;

  try {
    if (rawKey) {
      const record = await authenticateApiKey(rawKey);
      if (!record) {
        return Response.json({ ok: false, error: "Invalid API key" }, { status: 401 });
      }
      if (!hasScope(record.scopes, "usage:write")) {
        return Response.json(
          { ok: false, error: "Missing scope: usage:write" },
          { status: 403 },
        );
      }
      await ensureWorkspaceActive(record.workspaceId);
      workspaceId = record.workspaceId;
    } else if (session?.user?.id) {
      workspaceId = await requireActiveWorkspaceId(session.user.id);
      userId = session.user.id;
    } else {
      return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
    }
  } catch (err: any) {
    return Response.json(
      { ok: false, error: err?.message || "Workspace unavailable" },
      { status: 403 },
    );
  }

  try {
    await requireActiveSubscription(workspaceId);
  } catch (err: any) {
    return Response.json(
      { ok: false, error: err?.message || "Subscription required" },
      { status: 402 },
    );
  }

  const body = await request.json();
  const tokens = body?.tokens ? Number(body.tokens) : null;
  const costUsd = body?.costUsd ? Number(body.costUsd) : null;

  const anomalyTokenThreshold = Number(
    process.env.USAGE_ANOMALY_TOKEN_THRESHOLD || "0",
  );
  const anomalyCostThreshold = Number(
    process.env.USAGE_ANOMALY_COST_THRESHOLD || "0",
  );

  const enforceBudgets = process.env.ENFORCE_BUDGETS !== "false";
  const limitCheck = await checkUsageLimits(workspaceId, {
    tokens: Number.isFinite(tokens) ? tokens : 0,
    costUsd: Number.isFinite(costUsd) ? costUsd : 0,
  });
  if (enforceBudgets && !limitCheck.ok) {
    return Response.json(
      { ok: false, error: limitCheck.reason || "Budget exceeded" },
      { status: 402 },
    );
  }

  if (
    workspaceId &&
    ((Number.isFinite(anomalyTokenThreshold) && anomalyTokenThreshold > 0 && (tokens || 0) >= anomalyTokenThreshold) ||
      (Number.isFinite(anomalyCostThreshold) && anomalyCostThreshold > 0 && (costUsd || 0) >= anomalyCostThreshold))
  ) {
    await recordAuditLog({
      workspaceId,
      userId,
      action: "usage.anomaly",
      targetType: "usage_event",
      metadata: JSON.stringify({
        tokens,
        costUsd,
        model: body?.model || null,
        source: body?.source || null,
      }),
    });
  }

  await recordUsageEvent({
    workspaceId,
    userId,
    source: body?.source ? String(body.source) : null,
    model: body?.model ? String(body.model) : null,
    tokens: Number.isFinite(tokens) ? tokens : null,
    costUsd: Number.isFinite(costUsd) ? costUsd : null,
    metadata: body?.metadata ? JSON.stringify(body.metadata) : null,
  });

  const summary = await getUsageSummary(workspaceId);

  return Response.json({ ok: true, summary });
}
