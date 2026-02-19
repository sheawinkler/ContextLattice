import fs from "fs/promises";
import path from "path";
import { prisma } from "@/lib/db";

const exportDir = process.env.AUDIT_EXPORT_DIR || "../tmp/audit_exports";
const limit = Number(process.env.AUDIT_EXPORT_LIMIT || "5000");

async function main() {
  const resolvedDir = path.resolve(process.cwd(), exportDir);
  await fs.mkdir(resolvedDir, { recursive: true });

  const logs = await prisma.auditLog.findMany({
    orderBy: { createdAt: "desc" },
    take: limit,
  });

  const payload = logs.map((log) => ({
    id: log.id,
    workspaceId: log.workspaceId,
    userId: log.userId,
    action: log.action,
    targetType: log.targetType,
    targetId: log.targetId,
    metadata: log.metadata,
    createdAt: log.createdAt.toISOString(),
  }));

  const fileName = `audit_export_${new Date().toISOString().replace(/[:.]/g, "-")}.json`;
  const filePath = path.join(resolvedDir, fileName);
  await fs.writeFile(filePath, JSON.stringify(payload, null, 2));

  console.log(`[audit-export] wrote ${payload.length} rows to ${filePath}`);
}

main()
  .catch((err) => {
    console.error("[audit-export] error", err);
    process.exitCode = 1;
  })
  .finally(async () => {
    await prisma.$disconnect();
  });
