// The console's data layer.
//
// Interim mode per specification.md §4.5: every view polls its endpoints while
// visible and pauses while the tab is hidden. Components never see the polling —
// they get `{data, error, loading, refresh}` — so the SSE transport that task E1
// introduces can replace the inside of this hook without touching a view.
import { useCallback, useEffect, useRef, useState } from "react";
import { describeLoadError } from "./runner-status";

export const POLL_INTERVAL_MS = 10_000;

export type Polled<T> = {
  /** null until the first successful load; never null again afterwards. */
  data: T | null;
  /** Set whenever the most recent load or refresh failed. */
  error: string | null;
  loading: boolean;
  /** Abandons any in-flight request and starts a clean load. */
  refresh: () => void;
  update: (next: (current: T) => T) => void;
};

export type PollOptions = {
  intervalMs?: number;
  /** Set false to load once and never poll (detail sub-resources page instead). */
  poll?: boolean;
};

/**
 * Loads `load` now, then on an interval while the document is visible.
 *
 * `load` must be stable (wrap it in useCallback) — it is the effect's identity,
 * so a fresh function every render would restart the poll on every render.
 */
export function usePolled<T>(
  load: (signal: AbortSignal) => Promise<T>,
  fallbackMessage: string,
  options: PollOptions = {}
): Polled<T> {
  const { intervalMs = POLL_INTERVAL_MS, poll = true } = options;
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [reloadToken, setReloadToken] = useState(0);
  // Held in a ref, not a dependency: changing the message must not restart the poll.
  const fallback = useRef(fallbackMessage);
  useEffect(() => { fallback.current = fallbackMessage; }, [fallbackMessage]);

  const refresh = useCallback(() => setReloadToken((token) => token + 1), []);
  const update = useCallback((next: (current: T) => T) => {
    setData((current) => current === null ? current : next(current));
  }, []);

  useEffect(() => {
    let cancelled = false;
    // One request at a time. Without this, a slow orchestrator lets the poll
    // stack requests that then resolve out of order, and an older snapshot can
    // overwrite a newer one.
    let inFlight: AbortController | null = null;

    const run = (background: boolean) => {
      if (inFlight) return;
      const controller = new AbortController();
      inFlight = controller;
      if (!background) setLoading(true);

      load(controller.signal).then(
        (result) => {
          inFlight = null;
          if (cancelled) return;
          setData(result);
          setError(null);
          setLoading(false);
        },
        (reason: unknown) => {
          inFlight = null;
          if (cancelled || controller.signal.aborted) return;
          setError(describeLoadError(reason, fallback.current));
          setLoading(false);
        }
      );
    };

    run(false);

    if (!poll) {
      return () => {
        cancelled = true;
        inFlight?.abort();
      };
    }

    const interval = setInterval(() => {
      if (!document.hidden) run(true);
    }, intervalMs);
    // Refresh on the way back too: a tab left hidden for an hour would otherwise
    // show an hour-old console until the next tick.
    const onVisibility = () => {
      if (!document.hidden) run(true);
    };
    document.addEventListener("visibilitychange", onVisibility);

    return () => {
      cancelled = true;
      inFlight?.abort();
      clearInterval(interval);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [load, intervalMs, poll, reloadToken]);

  return { data, error, loading, refresh, update };
}
