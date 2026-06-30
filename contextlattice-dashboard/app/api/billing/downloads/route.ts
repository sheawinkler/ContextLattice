import { getDashboardSession } from "@/lib/dashboardSession";
import { requireUserWorkspaceId } from "@/lib/workspace";
import {
  configuredPremiumDownloads,
  defaultDownloadTokenTtlMinutes,
} from "@/lib/billing/downloads";
import { getWorkspacePlan, requireActiveSubscription } from "@/lib/billing/entitlements";

export async function GET() {
  const session = await getDashboardSession();
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

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
  const assets = configuredPremiumDownloads();
  if (!assets.length) {
    return Response.json(
      { ok: false, error: "Premium download artifacts are not configured" },
      { status: 503 },
    );
  }

  return Response.json({
    ok: true,
    planId: plan.planId,
    expiresInMinutesDefault: defaultDownloadTokenTtlMinutes(),
    assets: assets.map((asset) => ({
      id: asset.id,
      label: asset.label,
      fileName: asset.fileName,
      route: `/api/billing/download?asset=${asset.id}`,
    })),
  });
}
