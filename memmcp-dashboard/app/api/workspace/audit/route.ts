import { getServerSession } from "next-auth";
import { authOptions } from "@/lib/auth";
import { requireActiveWorkspaceId } from "@/lib/workspace";
import { listAuditLogs, recordAuditLog } from "@/lib/audit";
import { extractApiKey, authenticateApiKey, hasScope } from "@/lib/auth/apiKeys";

export async function GET(request: Request) {
  const session = await getServerSession(authOptions);
  if (!session?.user?.id) {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const workspaceId = await requireActiveWorkspaceId(session.user.id);
  const url = new URL(request.url);
  const limit = Math.min(Number(url.searchParams.get("limit") || 25), 200);
  const logs = await listAuditLogs(workspaceId, limit);

  return Response.json({ ok: true, logs });
}

export async function POST(request: Request) {
  const rawKey = extractApiKey(request);
  const session = await getServerSession(authOptions);
  let workspaceId: string | null = null;
  let userId: string | null = null;

  if (rawKey) {
    const record = await authenticateApiKey(rawKey);
    if (!record) {
      return Response.json({ ok: false, error: "Invalid API key" }, { status: 401 });
    }
    if (!hasScope(record.scopes, "audit:write")) {
      return Response.json(
        { ok: false, error: "Missing scope: audit:write" },
        { status: 403 },
      );
    }
    workspaceId = record.workspaceId;
  } else if (session?.user?.id) {
    workspaceId = await requireActiveWorkspaceId(session.user.id);
    userId = session.user.id;
  } else {
    return Response.json({ ok: false, error: "Unauthorized" }, { status: 401 });
  }

  const body = await request.json();
  const action = String(body?.action || "").trim();
  if (!action) {
    return Response.json({ ok: false, error: "action is required" }, { status: 400 });
  }

  await recordAuditLog({
    workspaceId,
    userId,
    action,
    targetType: body?.targetType ? String(body.targetType) : "ops",
    targetId: body?.targetId ? String(body.targetId) : null,
    metadata: body?.metadata ? JSON.stringify(body.metadata) : null,
  });

  return Response.json({ ok: true });
}
