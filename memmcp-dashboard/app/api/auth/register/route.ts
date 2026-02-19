import { hash } from "bcryptjs";
import { prisma } from "@/lib/db";

function slugify(value: string) {
  return value
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/(^-|-$)+/g, "")
    .slice(0, 40);
}

export async function POST(request: Request) {
  const body = await request.json();
  const email = String(body?.email || "").trim().toLowerCase();
  const password = String(body?.password || "");
  const name = String(body?.name || "").trim() || null;

  if (!email || !password) {
    return Response.json(
      { ok: false, error: "Email and password are required." },
      { status: 400 },
    );
  }
  if (password.length < 8) {
    return Response.json(
      { ok: false, error: "Password must be at least 8 characters." },
      { status: 400 },
    );
  }

  const existing = await prisma.user.findUnique({ where: { email } });
  if (existing) {
    return Response.json(
      { ok: false, error: "Account already exists." },
      { status: 409 },
    );
  }

  const passwordHash = await hash(password, 12);
  const baseSlug = slugify(name || email.split("@")[0] || "workspace") || "workspace";
  let slug = baseSlug;
  let suffix = 1;
  while (await prisma.workspace.findUnique({ where: { slug } })) {
    slug = `${baseSlug}-${suffix}`;
    suffix += 1;
  }

  const user = await prisma.$transaction(async (tx) => {
    const createdUser = await tx.user.create({
      data: {
        email,
        name,
        passwordHash,
      },
    });
    const workspace = await tx.workspace.create({
      data: {
        name: `${name || email.split("@")[0]}'s Workspace`,
        slug,
        ownerId: createdUser.id,
      },
    });
    await tx.workspaceMember.create({
      data: {
        workspaceId: workspace.id,
        userId: createdUser.id,
        role: "owner",
      },
    });
    await tx.auditLog.create({
      data: {
        workspaceId: workspace.id,
        userId: createdUser.id,
        action: "workspace.create",
        targetType: "workspace",
        targetId: workspace.id,
        metadata: JSON.stringify({ slug: workspace.slug }),
      },
    });
    return createdUser;
  });

  return Response.json({ ok: true, user: { id: user.id, email: user.email } });
}
