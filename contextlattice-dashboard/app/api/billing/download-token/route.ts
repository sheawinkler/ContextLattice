import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { requireUserWorkspaceId } from "@/lib/workspace";
import {
  appOrigin,
  configuredPremiumDownloads,
  defaultDownloadTokenTtlMinutes,
  issueDownloadToken,
  type PremiumDownloadAssetId,
} from "@/lib/billing/downloads";
import {
  getWorkspacePlan,
  requireActiveSubscription,
} from "@/lib/billing/entitlements";
import { sendEmail } from "@/lib/email";

function normalizeAsset(input: string): PremiumDownloadAssetId | null {
  const value = String(input || "").trim().toLowerCase();
  if (value === "macos" || value === "windows" || value === "linux") {
    return value;
  }
  return null;
}

export async function POST(request: Request) {
  const session = await getServerSession(authOptions);
  const userId = session?.user?.id || "";
  const userEmail = session?.user?.email || "";
  if (!userId || !userEmail) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const body = await request.json().catch(() => ({} as Record<string, unknown>));
  const requestedAsset = normalizeAsset(String(body.asset || ""));
  const requestedTtl = Number(body.ttlMinutes || defaultDownloadTokenTtlMinutes());
  const shouldEmail = body.email === true;

  const workspaceId = await requireUserWorkspaceId(userId);
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

  const selectedAssets = requestedAsset
    ? assets.filter((asset) => asset.id === requestedAsset)
    : assets;
  if (!selectedAssets.length) {
    return Response.json(
      { ok: false, error: "Requested artifact is not configured" },
      { status: 404 },
    );
  }

  const ttlMinutes = Math.min(24 * 60, Math.max(5, Math.floor(requestedTtl)));
  const origin = appOrigin().replace(/\/+$/, "");
  const links = selectedAssets.map((asset) => {
    const token = issueDownloadToken(asset.id, {
      ttlMinutes,
      userId,
      planId: plan.planId,
    });
    return {
      id: asset.id,
      label: asset.label,
      fileName: asset.fileName,
      expiresInMinutes: ttlMinutes,
      url: `${origin}/api/billing/download?asset=${asset.id}&token=${encodeURIComponent(
        token,
      )}`,
    };
  });

  let emailResult: { ok: boolean; error?: string } | null = null;
  if (shouldEmail) {
    const textLines = [
        `Your ContextLattice premium download links (${plan.planId} plan):`,
      "",
      ...links.map((link) => `- ${link.label}: ${link.url}`),
      "",
      `Links expire in ${ttlMinutes} minute(s).`,
    ];
    emailResult = await sendEmail({
      to: userEmail,
      subject: "ContextLattice premium download links",
      text: textLines.join("\n"),
      html: `
        <p>Your ContextLattice premium download links (<strong>${plan.planId}</strong> plan):</p>
        <ul>
          ${links
            .map(
              (link) =>
                `<li><a href="${link.url}">${link.label}</a> (${link.fileName})</li>`,
            )
            .join("")}
        </ul>
        <p>Links expire in ${ttlMinutes} minute(s).</p>
      `,
    });
  }

  return Response.json({
    ok: true,
    planId: plan.planId,
    ttlMinutes,
    links,
    email: emailResult,
  });
}
