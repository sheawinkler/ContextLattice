"use client";

import { useEffect, useRef, useState } from "react";
import { createDashboardRefresh, loadDashboardParts, type RefreshState } from "@/lib/dashboardRefresh";

const DEFAULT_OPTIONS = { retainLastGoodOnError: true };

export function useDashboardSnapshot<T extends Record<string, unknown>>(
  initialData: T, paths: Record<keyof T, string>, options = DEFAULT_OPTIONS,
) {
  const [snapshot, setSnapshot] = useState<RefreshState<T>>({
    data: initialData, errors: [], attemptedAt: null, succeededAt: null, refreshing: false, stale: true,
  });
  const ref = useRef<ReturnType<typeof createDashboardRefresh<T>> | null>(null);
  useEffect(() => {
    const controller = createDashboardRefresh((signal) => loadDashboardParts<T>(paths, signal), setSnapshot, {
      initialData, visible: () => document.visibilityState !== "hidden", ...options,
    });
    ref.current = controller;
    document.addEventListener("visibilitychange", controller.visibilityChanged);
    void controller.start();
    return () => {
      controller.stop();
      ref.current = null;
      document.removeEventListener("visibilitychange", controller.visibilityChanged);
    };
  }, [initialData, paths, options]);
  return { ...snapshot, refresh: () => ref.current?.refresh() };
}
