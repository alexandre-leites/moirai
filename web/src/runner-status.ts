import type { Runner } from "./api";
import { ApiError } from "./api";

const DEFAULT_HEARTBEAT_INTERVAL_MS = 10_000;

function configuredHeartbeatIntervalMs(): number {
  const parsed = Number(import.meta.env.VITE_RUNNER_HEARTBEAT_INTERVAL_MS);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : DEFAULT_HEARTBEAT_INTERVAL_MS;
}

// A runner reports on LOOP_RUNNER_HEARTBEAT_INTERVAL, 10s by default
// (runner/README.md). That value is per-runner env configuration and
// `GET /api/v1/runners` does not report it, so the page has to assume one:
// the default, overridable at build time for a fleet that does not use it.
// Without the override a fleet configured to, say, 60s would render every
// runner permanently stale. Once the payload carries the interval
// (docs/design/web-console/tasks.md A12) this guess goes away.
export const HEARTBEAT_INTERVAL_MS = configuredHeartbeatIntervalMs();

// The console specification treats a probe as stale after three of its own
// intervals have passed without a report (docs/design/web-console/
// specification.md §5.1), so three missed heartbeats is the fleet's staleness
// budget: long enough that one dropped beat or a reconnect is not reported as
// a fault, short enough that a runner whose process died is visible before the
// next scheduling decision.
export const MISSED_HEARTBEATS_BEFORE_STALE = 3;
export const STALE_AFTER_MS = MISSED_HEARTBEATS_BEFORE_STALE * HEARTBEAT_INTERVAL_MS;

export type PillVariant = "ok" | "warn" | "bad" | "idle";

export type RunnerStatus = {
  label: string;
  variant: PillVariant;
};

export type HeartbeatAge = {
  /** Human-readable age, e.g. "8s ago". */
  label: string;
  /** Absolute timestamp for the `title` attribute, or an explanation when there is none. */
  title: string;
  /** True when the runner missed its heartbeat budget, never reported, or reported garbage. */
  stale: boolean;
};

/**
 * Renders an elapsed duration the way an operator reads it. Negative ages
 * collapse to "just now" rather than rendering a nonsense future age: the
 * timestamp is stamped by the orchestrator and compared against the browser's
 * clock, so the two disagreeing by a few seconds is normal.
 */
export function formatAge(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  if (seconds < 5) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

/**
 * Maps `lastSeenAt` to the heartbeat cell. A missing or unreadable timestamp
 * is stale: the console has no evidence the runner is alive, which is exactly
 * what the operator needs to see. The two cases are told apart, because
 * "never reported" is an operational fact and "unreadable" is a defect.
 */
export function describeHeartbeat(lastSeenAt: string | null | undefined, nowMs: number): HeartbeatAge {
  if (lastSeenAt === null || lastSeenAt === undefined || lastSeenAt === "") {
    return {
      label: "never",
      title: "This runner has never reported a heartbeat.",
      stale: true,
    };
  }
  const seenMs = Date.parse(lastSeenAt);
  if (Number.isNaN(seenMs)) {
    return {
      label: "unknown",
      title: `The orchestrator reported a heartbeat time this page cannot read: ${lastSeenAt}`,
      stale: true,
    };
  }
  const ageMs = nowMs - seenMs;
  return {
    label: formatAge(ageMs),
    title: new Date(seenMs).toLocaleString(),
    stale: ageMs > STALE_AFTER_MS,
  };
}

/**
 * Status pill per specification §5.5 (Online / Draining / Offline), with two
 * additions the current payload forces:
 *
 * - Stale outranks everything except an explicit offline row. `runners.status`
 *   is a lagging column: the orchestrator sets it to `online` on every
 *   heartbeat but only back to `offline` through lease expiry (600s, and only
 *   for a runner holding a lease) or revocation. An *idle* runner that is
 *   killed therefore keeps `status = 'online'` indefinitely. Reading the pill
 *   off that column alone would show a green "Online" beside "7d ago", the
 *   opposite of what this page exists to tell an operator. Warn, not crit,
 *   because §5.1 assigns warn to a stale probe.
 * - `enabled = false` has no pill in the spec, but a connected runner an
 *   operator has disabled will never be offered work, and the fleet view is
 *   where that has to be visible.
 */
export function describeRunnerStatus(runner: Runner, heartbeat: HeartbeatAge): RunnerStatus {
  if (runner.status !== "online") {
    if (runner.status === "offline") return { label: "Offline", variant: "bad" };
    // A status added server-side later: name it rather than mislabel it, and
    // stay neutral instead of painting the fleet critical over a new word.
    const label = runner.status ? runner.status[0].toUpperCase() + runner.status.slice(1) : "Unknown";
    return { label, variant: "idle" };
  }
  if (heartbeat.stale) return { label: "Stale", variant: "warn" };
  if (runner.draining) return { label: "Draining", variant: "warn" };
  if (!runner.enabled) return { label: "Disabled", variant: "idle" };
  return { label: "Online", variant: "ok" };
}

/**
 * Runners the orchestrator can currently reach. A stale heartbeat does not
 * count, for the reason described on `describeRunnerStatus`: `status` alone
 * would report a killed idle runner as online forever.
 */
export function countOnline(runners: Runner[], nowMs: number): number {
  return runners.filter(
    (runner) => runner.status === "online" && !describeHeartbeat(runner.lastSeenAt, nowMs).stale
  ).length;
}

/**
 * Turns a rejection into the sentence the view shows. `ApiError.message`
 * already carries the problem+json `title: detail`, so it is used verbatim for
 * anything the API answered; 401 and 403 get their own copy because the generic
 * problem titles ("Unauthorized") do not tell an operator what to do next.
 */
export function describeLoadError(error: unknown, fallback = "The control plane could not be reached."): string {
  if (error instanceof ApiError) {
    if (error.status === 401) return "Your session has expired. Sign in again.";
    if (error.status === 403) return "Your account is not allowed to see this.";
    return error.message;
  }
  if (error instanceof Error && error.message) return error.message;
  if (typeof error === "string" && error) return error;
  return fallback;
}
