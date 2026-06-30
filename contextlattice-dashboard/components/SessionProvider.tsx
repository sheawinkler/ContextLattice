"use client";

import { SessionProvider } from "next-auth/react";
import type { ReactNode } from "react";

export function AuthProvider({
  children,
  enabled,
}: {
  children: ReactNode;
  enabled: boolean;
}) {
  if (!enabled) {
    return <>{children}</>;
  }
  return <SessionProvider>{children}</SessionProvider>;
}
