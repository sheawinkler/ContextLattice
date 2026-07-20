"use client";

import type { ReactNode } from "react";
import { AuthProvider } from "@/components/SessionProvider";
import { ShellNav } from "@/components/ShellNav";
import {
  DashboardAuthModeProvider,
  useDashboardAuthRequired,
} from "@/lib/useDashboardAuthMode";

export function DashboardShell({ children }: { children: ReactNode }) {
  const authMode = useDashboardAuthRequired();
  const authResolved = typeof authMode === "boolean";
  const authEnabled = authMode === true;

  return (
    <DashboardAuthModeProvider value={authMode}>
      <AuthProvider enabled={authEnabled}>
        <header className="shell-header">
          <div className="shell-header-row">
            <div className="shell-title-wrap">
              <h1 className="shell-title">ContextLattice</h1>
              <p className="shell-subtitle">
                Local-first agent intelligence. Continuity, context, and proof.
              </p>
            </div>
            <ShellNav authEnabled={authEnabled} authResolved={authResolved} />
          </div>
        </header>
        <main className="shell-main">{children}</main>
      </AuthProvider>
    </DashboardAuthModeProvider>
  );
}
