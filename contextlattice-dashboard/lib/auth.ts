import type { NextAuthOptions } from "next-auth";
import Credentials from "next-auth/providers/credentials";
import GitHubProvider from "next-auth/providers/github";
import GoogleProvider from "next-auth/providers/google";
import AppleProvider from "next-auth/providers/apple";
import { PrismaAdapter } from "@next-auth/prisma-adapter";
import { compare } from "bcryptjs";
import { prisma } from "./db";
import { isRateLimited, recordAttempt as recordFailedAttempt } from "./rateLimit";
import { sanitizeCallbackUrl } from "./callback-url";

export const authOptions: NextAuthOptions = {
  adapter: PrismaAdapter(prisma),
  // Credentials auth in NextAuth requires JWT sessions.
  session: { strategy: "jwt" },
  providers: [
    Credentials({
      name: "Email & Password",
      credentials: {
        email: { label: "Email", type: "email" },
        password: { label: "Password", type: "password" },
      },
      async authorize(credentials) {
        if (!credentials?.email || !credentials?.password) {
          return null;
        }
        const email = credentials.email.trim().toLowerCase();
        if (await isRateLimited(email, "login")) {
          throw new Error("RATE_LIMITED");
        }
        const user = await prisma.user.findUnique({
          where: { email },
        });
        if (!user?.passwordHash) {
          await recordFailedAttempt(email, "login");
          return null;
        }
        const valid = await compare(credentials.password, user.passwordHash);
        if (!valid) {
          await recordFailedAttempt(email, "login");
          return null;
        }
        if (
          process.env.AUTH_REQUIRE_EMAIL_VERIFICATION === "true" &&
          !user.emailVerified
        ) {
          await recordFailedAttempt(email, "login");
          throw new Error("EMAIL_NOT_VERIFIED");
        }
        return {
          id: user.id,
          email: user.email,
          name: user.name,
        };
      },
    }),
    ...(process.env.GITHUB_ID && process.env.GITHUB_SECRET
      ? [
          GitHubProvider({
            clientId: process.env.GITHUB_ID,
            clientSecret: process.env.GITHUB_SECRET,
          }),
        ]
      : []),
    ...(process.env.GOOGLE_CLIENT_ID && process.env.GOOGLE_CLIENT_SECRET
      ? [
          GoogleProvider({
            clientId: process.env.GOOGLE_CLIENT_ID,
            clientSecret: process.env.GOOGLE_CLIENT_SECRET,
          }),
        ]
      : []),
    ...(process.env.APPLE_OAUTH_ENABLED === "true" &&
      process.env.APPLE_ID &&
      process.env.APPLE_CLIENT_SECRET
      ? [
          AppleProvider({
            clientId: process.env.APPLE_ID,
            clientSecret: process.env.APPLE_CLIENT_SECRET,
          }),
        ]
      : []),
  ],
  pages: {
    signIn: "/auth/login",
  },
  callbacks: {
    async redirect({ url, baseUrl }) {
      const redirectUrl = sanitizeCallbackUrl(url, "/overview");
      if (redirectUrl !== "/overview") {
        return `${baseUrl}${redirectUrl}`;
      }
      return `${baseUrl}/overview`;
    },

    async jwt({ token, user }) {
      const userId =
        (typeof user?.id === "string" && user.id) ||
        (typeof token.sub === "string" && token.sub) ||
        "";
      if (userId) {
        token.id = userId;
        const membership = await prisma.workspaceMember.findFirst({
          where: { userId },
          orderBy: { createdAt: "asc" },
        });
        token.workspaceId = membership?.workspaceId || null;
        token.workspaceRole = membership?.role || null;
      }
      return token;
    },
    async session({ session, token }) {
      if (session.user) {
        const user = session.user as typeof session.user & {
          id?: string;
          workspaceId?: string | null;
          workspaceRole?: string | null;
        };
        user.id =
          (typeof token.id === "string" && token.id) ||
          (typeof token.sub === "string" && token.sub) ||
          undefined;
        user.workspaceId =
          typeof token.workspaceId === "string" ? token.workspaceId : null;
        user.workspaceRole =
          typeof token.workspaceRole === "string" ? token.workspaceRole : null;
      }
      return session;
    },
  },
};
