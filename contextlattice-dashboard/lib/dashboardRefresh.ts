export type RefreshState<T> = {
  data: T;
  errors: string[];
  attemptedAt: string | null;
  succeededAt: string | null;
  refreshing: boolean;
  stale: boolean;
};

type RefreshOptions<T> = {
  initialData: T;
  intervalMs?: number;
  deadlineMs?: number;
  retainLastGoodOnError?: boolean;
  now?: () => number;
  visible?: () => boolean;
  setTimer?: (fn: () => void, delay: number) => unknown;
  clearTimer?: (id: unknown) => void;
};

export function createDashboardRefresh<T>(
  load: (signal: AbortSignal) => Promise<{ data: T; errors: string[] }>,
  changed: (state: RefreshState<T>) => void,
  options: RefreshOptions<T>,
) {
  const interval = options.intervalMs ?? 300000;
  const deadline = options.deadlineMs ?? 20000;
  const now = options.now ?? Date.now;
  const visible = options.visible ?? (() => true);
  const setTimer = options.setTimer ?? ((fn, delay) => setTimeout(fn, delay));
  const clearTimer = options.clearTimer ?? ((id) => clearTimeout(id as ReturnType<typeof setTimeout>));
  let state: RefreshState<T> = { data: options.initialData, errors: [], attemptedAt: null, succeededAt: null, refreshing: false, stale: true };
  let active = false;
  let generation = 0;
  let failures = 0;
  let timer: unknown;
  let abort: AbortController | null = null;
  let flight: Promise<void> | null = null;
  const publish = (patch: Partial<RefreshState<T>>) => { state = { ...state, ...patch }; changed(state); };
  const cancelTimer = () => { if (timer !== undefined) clearTimer(timer); timer = undefined; };

  function refresh(): Promise<void> {
    if (!active) return Promise.resolve();
    if (flight) return flight;
    cancelTimer();
    const ticket = ++generation;
    const controller = new AbortController();
    abort = controller;
    publish({ attemptedAt: new Date(now()).toISOString(), refreshing: true });
    let timeout: unknown;
    let onAbort: () => void = () => {};
    const bounded = new Promise<never>((_, reject) => {
      onAbort = () => reject(controller.signal.reason ?? new Error("Dashboard refresh cancelled"));
      controller.signal.addEventListener("abort", onAbort, { once: true });
      timeout = setTimer(() => controller.abort(new Error("Dashboard refresh timed out")), deadline);
    });
    let request: Promise<{ data: T; errors: string[] }>;
    try { request = load(controller.signal); }
    catch (error) { request = Promise.reject(error); }
    flight = Promise.race([request, bounded]).then((result) => {
      if (!active || ticket !== generation) return;
      if (result.errors.length) {
        failures++;
        publish({ data: options.retainLastGoodOnError === false || state.succeededAt === null ? result.data : state.data, errors: result.errors, stale: true });
      } else {
        failures = 0;
        publish({ data: result.data, errors: [], stale: false, succeededAt: new Date(now()).toISOString() });
      }
    }, (error: unknown) => {
      if (!active || ticket !== generation) return;
      failures++;
      publish({ data: options.retainLastGoodOnError === false ? options.initialData : state.data,
        errors: [error instanceof Error ? error.message : "Dashboard refresh failed"], stale: true });
    }).finally(() => {
      clearTimer(timeout);
      controller.signal.removeEventListener("abort", onAbort);
      if (!active || ticket !== generation) return;
      abort = null;
      flight = null;
      publish({ refreshing: false });
      if (visible()) timer = setTimer(() => { void refresh(); }, Math.min(interval * 2 ** Math.min(failures, 3), 1800000));
    });
    return flight;
  }

  return {
    refresh,
    start() { active = true; return visible() ? refresh() : Promise.resolve(); },
    visibilityChanged() {
      if (!active) return;
      if (visible()) { void refresh(); return; }
      cancelTimer();
      generation++;
      abort?.abort();
      abort = null;
      flight = null;
      publish({ refreshing: false, stale: true });
    },
    stop() {
      active = false;
      generation++;
      cancelTimer();
      abort?.abort();
      abort = null;
      flight = null;
    },
  };
}

export async function loadDashboardParts<T extends Record<string, unknown>>(
  paths: Record<keyof T, string>, signal: AbortSignal,
): Promise<{ data: T; errors: string[] }> {
  const entries = await Promise.all(Object.entries(paths).map(async ([key, path]) => {
    try {
      const response = await fetch(path, { cache: "no-store", signal });
      if (!response.ok) throw new Error(`${path} -> ${response.status}`);
      const data = await response.json();
      if (!data || typeof data !== "object" || Array.isArray(data)) throw new Error(`${path} -> invalid snapshot`);
      const upstreamErrors = data.errors && typeof data.errors === "object" ? Object.values(data.errors).map(String) : [];
      return { key, data, errors: upstreamErrors };
    } catch (error) {
      return { key, data: null, errors: [error instanceof Error ? error.message : `${path} -> fetch failed`] };
    }
  }));
  return { data: Object.fromEntries(entries.map(({ key, data }) => [key, data])) as T, errors: entries.flatMap((entry) => entry.errors) };
}
