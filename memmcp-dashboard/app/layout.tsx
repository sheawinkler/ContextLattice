import type { ReactNode } from "react";
import "./globals.css";
import { AuthProvider } from "@/components/SessionProvider";

export const metadata = {
  title: "ContextLattice Console",
  description: "Operator console for the memory and context stack",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body className="bg-slate-950 text-slate-100 min-h-screen">
        <AuthProvider>
          <header className="border-b border-slate-800 p-4">
            <div className="flex flex-wrap items-start justify-between gap-4">
              <div>
                <h1 className="text-2xl font-semibold">ContextLattice Console</h1>
                <p className="text-sm text-slate-400">
                  Live window into the memory bank, orchestrator, and MCP stack
                </p>
              </div>
              <nav className="flex flex-wrap gap-3 text-sm text-slate-300">
                <a className="hover:text-emerald-300" href="/">
                  Console
                </a>
                <a className="hover:text-emerald-300" href="/status">
                  Status
                </a>
                <a className="hover:text-emerald-300" href="/setup">
                  Setup
                </a>
                <a className="hover:text-emerald-300" href="/pricing">
                  Pricing
                </a>
                <a className="hover:text-emerald-300" href="/billing">
                  Billing
                </a>
                <a className="hover:text-emerald-300" href="/settings">
                  Settings
                </a>
                <a className="hover:text-emerald-300" href="/auth/login">
                  Sign in
                </a>
              </nav>
            </div>
          </header>
          <main className="p-4 max-w-5xl mx-auto">{children}</main>
        </AuthProvider>
      </body>
    </html>
  );
}
