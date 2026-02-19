import { prisma } from "@/lib/db";

export async function getUserWorkspaceId(userId: string) {
  const membership = await prisma.workspaceMember.findFirst({
    where: { userId },
    orderBy: { createdAt: "asc" },
  });
  return membership?.workspaceId || null;
}

export async function requireUserWorkspaceId(userId: string) {
  const workspaceId = await getUserWorkspaceId(userId);
  if (!workspaceId) {
    throw new Error("Workspace not found");
  }
  return workspaceId;
}

export async function requireActiveWorkspaceId(userId: string) {
  const membership = await prisma.workspaceMember.findFirst({
    where: { userId },
    orderBy: { createdAt: "asc" },
    include: { workspace: true },
  });
  if (!membership?.workspace) {
    throw new Error("Workspace not found");
  }
  if (membership.workspace.status !== "active") {
    throw new Error("Workspace is not active");
  }
  return membership.workspace.id;
}

export async function ensureWorkspaceActive(workspaceId: string) {
  const workspace = await prisma.workspace.findUnique({ where: { id: workspaceId } });
  if (!workspace) {
    throw new Error("Workspace not found");
  }
  if (workspace.status !== "active") {
    throw new Error("Workspace is not active");
  }
  return workspace;
}

export async function getWorkspaceById(workspaceId: string) {
  return prisma.workspace.findUnique({ where: { id: workspaceId } });
}
