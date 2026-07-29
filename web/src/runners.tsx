import { useCallback, useEffect, useState } from "react";
import type { ApiClient, Runner } from "./api";
import {
  countOnline,
  describeHeartbeat,
  describeRunnerStatus,
  loadRunners,
  POLL_INTERVAL_MS,
  STALE_AFTER_MS,
} from "./runner-status";

const STALE_HINT = `No heartbeat for more than ${Math.round(STALE_AFTER_MS / 1000)}s.`;

export type RunnersViewProps = {
  /** null until the first successful load; never null again afterwards. */
  runners: Runner[] | null;
  /** Set whenever the most recent load or refresh failed. */
  error: string | null;
  loading: boolean;
  /** Reference time the heartbeat ages are measured against. */
  now: number;
  onRefresh: () => void;
};

function LabelList({ labels }: { labels: string[] }) {
  if (labels.length === 0) return <span className="muted">none</span>;
  return (
    <>
      {labels.map((label) => (
        <span key={label} className="chip mono">{label}</span>
      ))}
    </>
  );
}

function RunnerRows({ runners, now }: { runners: Runner[]; now: number }) {
  return (
    <>
      {runners.map((runner) => {
        const heartbeat = describeHeartbeat(runner.lastSeenAt, now);
        const status = describeRunnerStatus(runner, heartbeat);
        return (
          <tr key={runner.id} className={heartbeat.stale ? "runner-row--stale" : undefined}>
            <td>
              <div>{runner.name}</div>
              <div className="mono muted">{runner.id.slice(0, 8)}</div>
            </td>
            <td><span className={`pill pill--${status.variant}`}>{status.label}</span></td>
            <td><LabelList labels={runner.labels} /></td>
            <td>
              {runner.draining
                ? <span className="pill pill--warn">Draining</span>
                : <span className="muted">No</span>}
            </td>
            <td>
              <span className="mono" title={heartbeat.title}>{heartbeat.label}</span>
              {heartbeat.stale && (
                <span className="pill pill--warn stale-badge" title={STALE_HINT}>Stale</span>
              )}
            </td>
          </tr>
        );
      })}
    </>
  );
}

/**
 * The runner fleet, without any data fetching of its own, so every state it can
 * be in is directly renderable in a test. Read-only: drain, disable and revoke
 * controls need `POST /api/v1/runners/{id}/state`, which does not exist yet
 * (docs/design/web-console/tasks.md B1).
 */
export function RunnersView({ runners, error, loading, now, onRefresh }: RunnersViewProps) {
  return (
    <div>
      <div className="page-header">
        <h2>Runners</h2>
        <div className="page-header-aside">
          {runners && runners.length > 0 && (
            <span className="page-hint mono" aria-live="polite">
              {countOnline(runners, now)}/{runners.length} online
            </span>
          )}
          {/* Never disabled: a request that hangs must not lock the operator
              out of retrying, and a click restarts the load from scratch. */}
          <button onClick={onRefresh}>{loading ? "Refreshing..." : "Refresh"}</button>
        </div>
      </div>

      {error && (
        <div className="error" role="alert">
          <p>
            {runners
              ? "The runner fleet could not be refreshed, so what is shown below may be out of date."
              : "The runner fleet could not be loaded."}
          </p>
          <p>{error}</p>
          <button onClick={onRefresh}>Retry</button>
        </div>
      )}

      {runners === null
        ? (!error && <p>Loading runners...</p>)
        : runners.length === 0
          ? (
            <p className="empty-state">
              No runner is registered. Issue a runner token, then start a runner against this
              orchestrator to see it here.
            </p>
          )
          : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Runner</th>
                    <th>Status</th>
                    <th>Labels</th>
                    <th>Draining</th>
                    <th>Last heartbeat</th>
                  </tr>
                </thead>
                <tbody>
                  <RunnerRows runners={runners} now={now} />
                </tbody>
              </table>
            </div>
          )}
    </div>
  );
}

export function RunnersPage({ api }: { api: ApiClient }) {
  const [runners, setRunners] = useState<Runner[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [now, setNow] = useState(() => Date.now());
  // Bumped by the Refresh/Retry button to re-run the effect below, which
  // abandons whatever request is in flight and starts a clean one.
  const [refreshToken, setRefreshToken] = useState(0);

  const onRefresh = useCallback(() => setRefreshToken((token) => token + 1), []);

  useEffect(() => {
    let cancelled = false;
    // One request at a time. Without this, a slow orchestrator lets the poll
    // stack requests that then resolve out of order, and an older snapshot can
    // overwrite a newer one.
    let inFlight: AbortController | null = null;

    const refresh = (background: boolean) => {
      if (inFlight) return;
      const ctrl = new AbortController();
      inFlight = ctrl;
      // Sampled before the request, not after: heartbeat ages are relative to
      // when the orchestrator was asked. Sampling on arrival would add the
      // whole round-trip to every age and report a healthy fleet as stale
      // exactly when the orchestrator is slow.
      const requestedAt = Date.now();
      if (!background) setLoading(true);

      void loadRunners(api, ctrl.signal).then((result) => {
        inFlight = null;
        if (cancelled) return;
        setNow(requestedAt);
        if (result.kind === "error") {
          setError(result.message);
        } else {
          setRunners(result.runners);
          setError(null);
        }
        setLoading(false);
      });
    };

    refresh(false);

    // Interim polling per specification §4.5 — paused while the tab is hidden.
    const interval = setInterval(() => {
      if (!document.hidden) refresh(true);
    }, POLL_INTERVAL_MS);
    const onVisibilityChange = () => {
      if (!document.hidden) refresh(true);
    };
    document.addEventListener("visibilitychange", onVisibilityChange);

    return () => {
      cancelled = true;
      inFlight?.abort();
      clearInterval(interval);
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  }, [api, refreshToken]);

  return (
    <RunnersView runners={runners} error={error} loading={loading} now={now} onRefresh={onRefresh} />
  );
}
