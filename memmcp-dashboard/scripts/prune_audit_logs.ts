import { prisma } from "@/lib/db";

const retentionDays = Number(process.env.AUDIT_RETENTION_DAYS || "90");

async function main() {
  if (!Number.isFinite(retentionDays) || retentionDays <= 0) {
    console.error("[audit-prune] invalid AUDIT_RETENTION_DAYS");
    process.exitCode = 1;
    return;
  }

  const cutoff = new Date(Date.now() - retentionDays * 24 * 60 * 60 * 1000);
  const result = await prisma.auditLog.deleteMany({
    where: { createdAt: { lt: cutoff } },
  });
  console.log(
    `[audit-prune] deleted ${result.count} logs older than ${retentionDays} days`,
  );
}

main()
  .catch((err) => {
    console.error("[audit-prune] error", err);
    process.exitCode = 1;
  })
  .finally(async () => {
    await prisma.$disconnect();
  });
