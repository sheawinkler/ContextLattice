import { NextResponse } from "next/server";
import { getDashboardSession } from "@/lib/dashboardSession";
import { callOrchestrator } from "@/lib/orchestrator";
import { extractApiKey, authenticateApiKey, hasScope } from "@/lib/auth/apiKeys";
import { ensureWorkspaceActive, requireActiveWorkspaceId } from "@/lib/workspace";
import { checkUsageLimits } from "@/lib/usage/budgets";
import { estimateTokensFromText, recordUsageEvent } from "@/lib/usage/events";
import { recordAuditLog } from "@/lib/audit";

export async function POST(request: Request) {
  const session = await getDashboardSession();
  const rawKey = extractApiKey(request);
  let workspaceId: string | null = null;
  let userId: string | null = null;

  try {
    if (rawKey) {
      const key = await authenticateApiKey(rawKey);
      if (!key) {
        return NextResponse.json({ ok: false, error: "Invalid API key" }, { status: 401 });
      }
      if (!hasScope(key.scopes, "memory:write")) {
        return NextResponse.json(
          { ok: false, error: "Missing scope: memory:write" },
          { status: 403 },
        );
      }
      await ensureWorkspaceActive(key.workspaceId);
      workspaceId = key.workspaceId;
    } else if (session?.user?.id) {
      workspaceId = await requireActiveWorkspaceId(session.user.id);
      userId = session.user.id;
    } else {
      return NextResponse.json({ ok: false, error: "Unauthorized" }, { status: 401 });
    }
  } catch (err: any) {
    return NextResponse.json(
      { ok: false, error: err?.message || "Workspace unavailable" },
      { status: 403 },
    );
  }

  const body = await request.json();
  const projectName = String(body?.projectName || "").trim();
  if (!projectName) {
    return NextResponse.json({ ok: false, error: "projectName is required" }, { status: 400 });
  }
  const content = typeof body?.content === "string" ? body.content : "";
  const tokens = estimateTokensFromText(content);
  const enforceBudgets = process.env.ENFORCE_BUDGETS !== "false";
  if (enforceBudgets && workspaceId) {
    const limits = await checkUsageLimits(workspaceId, { tokens });
    if (!limits.ok) {
      return NextResponse.json(
        { ok: false, error: limits.reason || "Budget exceeded" },
        { status: 402 },
      );
    }
  }

  const data = await callOrchestrator("/memory/write", {
    method: "POST",
    body: JSON.stringify(body),
    headers: {
      "x-request-id": request.headers.get("x-request-id") || crypto.randomUUID(),
    },
  });

  if (workspaceId) {
    await recordUsageEvent({
      workspaceId,
      userId,
      source: "memory.write",
      tokens,
      metadata: JSON.stringify({
        project: body?.projectName,
        file: body?.fileName,
        bytes: content.length,
      }),
    });
    if (process.env.AUDIT_MEMORY_WRITES === "true") {
      await recordAuditLog({
        workspaceId,
        userId,
        action: "memory.write",
        targetType: "memory",
        metadata: JSON.stringify({
          project: body?.projectName,
          file: body?.fileName,
          bytes: content.length,
        }),
      });
    }
  }
  return NextResponse.json(data);
}
