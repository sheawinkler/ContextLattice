import { getDashboardSession } from "@/lib/dashboardSession";
import { requireUserWorkspaceId } from "@/lib/workspace";
import { requireActiveSubscription } from "@/lib/billing/entitlements";
import { premiumDownloadById, verifyDownloadToken } from "@/lib/billing/downloads";

function noStoreHeaders() {
  return {
    "cache-control": "no-store, max-age=0",
    pragma: "no-cache",
  };
}

export async function GET(request: Request) {
  const { searchParams } = new URL(request.url);
  const assetId = String(searchParams.get("asset") || "").trim().toLowerCase();
  const token = String(searchParams.get("token") || "").trim();

  const asset = premiumDownloadById(assetId);
  if (!asset) {
    return Response.json(
      { ok: false, error: "Unknown or unconfigured artifact asset" },
      { status: 404, headers: noStoreHeaders() },
    );
  }

  if (token) {
    const payload = verifyDownloadToken(token);
    if (!payload || payload.a !== asset.id) {
      return Response.json(
        { ok: false, error: "Invalid or expired download token" },
        { status: 401, headers: noStoreHeaders() },
      );
    }
    return Response.redirect(asset.url, 302);
  }

  const session = await getDashboardSession();
  if (!session?.user?.id) {
    return Response.json(
      { ok: false, error: "Unauthorized" },
      { status: 401, headers: noStoreHeaders() },
    );
  }

  const workspaceId = await requireUserWorkspaceId(session.user.id);
  try {
    await requireActiveSubscription(workspaceId);
  } catch (err: any) {
    return Response.json(
      { ok: false, error: err?.message || "Active subscription required" },
      { status: 402, headers: noStoreHeaders() },
    );
  }

  return Response.redirect(asset.url, 302);
}
