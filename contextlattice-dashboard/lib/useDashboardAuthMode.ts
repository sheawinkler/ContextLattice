"use client";

import {
  createContext,
  createElement,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";

export type DashboardAuthMode = boolean | null | "unavailable";
const DashboardAuthModeContext = createContext<DashboardAuthMode | undefined>(undefined);

export function DashboardAuthModeProvider({
  children,
  value,
}: {
  children: ReactNode;
  value: DashboardAuthMode;
}) {
  return createElement(DashboardAuthModeContext.Provider, { value }, children);
}

export function useDashboardAuthRequired(): DashboardAuthMode {
  const inherited = useContext(DashboardAuthModeContext);
  const [authRequired, setAuthRequired] = useState<DashboardAuthMode>(null);

  useEffect(() => {
    if (inherited !== undefined) return;
    let active = true;
    fetch("/api/public/auth-mode", { cache: "no-store" })
      .then(async (response) => {
        if (!response.ok) throw new Error("auth mode unavailable");
        const payload = await response.json();
        if (active) setAuthRequired(payload?.authRequired === true);
      })
      .catch(() => {
        if (active) setAuthRequired("unavailable");
      });
    return () => {
      active = false;
    };
  }, [inherited]);

  return inherited === undefined ? authRequired : inherited;
}
