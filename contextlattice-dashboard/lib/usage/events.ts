import { prisma } from "@/lib/db";

export function estimateTokensFromText(content: string) {
  const trimmed = content.trim();
  if (!trimmed) {
    return 0;
  }
  return Math.ceil(trimmed.length / 4);
}

export async function recordUsageEvent(input: {
  workspaceId: string;
  userId?: string | null;
  source?: string | null;
  model?: string | null;
  tokens?: number | null;
  costUsd?: number | null;
  metadata?: string | null;
}) {
  return prisma.usageEvent.create({
    data: {
      workspaceId: input.workspaceId,
      userId: input.userId ?? null,
      source: input.source ?? null,
      model: input.model ?? null,
      tokens: input.tokens ?? null,
      costUsd: input.costUsd ?? null,
      metadata: input.metadata ?? null,
    },
  });
}
