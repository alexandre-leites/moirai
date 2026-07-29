import { ApiError } from "./api";
import type { ApiClient, Runner } from "./api";

// A runner reports on LOOP_RUNNER_HEARTBEAT_INTERVAL, 10s by default
// (runner/README.md). The console specification treats a probe as stale after
// three of its own intervals have passed without a report
// (docs/design/web-console/specification.md §5.1), so three missed heartbeats
// is the fleet's staleness budget: long enough that a single dropped beat or a
// reconnect is not reported as a fault, short enough that a runner whose
// process died is visible before the next scheduling decision.
export const HEARTBEAT_INTERVAL_MS = 10_000;
export const MISSED_HEARTBEATS_BEFORE_STALE = 3;
export const STALE_AFTER_MS = MISSED_HEARTBEATS_BEFORE_STALE * HEARTBEAT_INTERVAL_MS;

// Interim refresh cadence for views, per specification §4.5 ("every view polls
// its endpoints on a 10s interval while visible"), until SSE ships.
export const POLL_INTERVAL_MS = 10_000;

export type PillVariant = "ok" | "warn" | "bad" | "idle";

export type RunnerStatus = {
  label: string;
  variant: PillVariant;
};

export type HeartbeatAge = {
  /** Human-readable age, e.g. "8s ago". "never" when the runner has not reported. */
  label: string;
  /** Absolute timestamp for the `title` attribute, or an explanation when unknown. */
  title: string;
  /** True when the runner has missed its heartbeat budget, or never reported at all. */
  stale: boolean;
};

export type RunnersResult =
  | { kind: "ready"; runners: Runner[] }
  | { kind: "error"; message: string };

/**
 * Renders an elapsed duration the way an operator reads it. Negative ages
 * (a runner clock slightly ahead of the browser's) collapse to "just now"
 * rather than rendering a nonsense future age.
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
 * Maps `lastSeenAt` to the heartbeat cell. A missing, empty or unparseable
 * timestamp is stale: the orchestrator has no evidence the runner is alive,
 * which is exactly what the operator needs to see.
 */
export function describeHeartbeat(lastSeenAt: string | undefined, nowMs: number): HeartbeatAge {
  const seenMs = Date.parse(lastSeenAt ?? "");
  if (Number.isNaN(seenMs)) {
    return {
      label: "never",
      title: "This runner has never reported a heartbeat.",
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
 * Status pill per specification §5.5: Online / Draining / Offline. `enabled`
 * has no pill in the spec, so a connected runner that an operator has disabled
 * reads as "Disabled" — it is online but will not be offered work, and the
 * fleet view is where that has to be visible.
 */
export function describeRunnerStatus(runner: Runner): RunnerStatus {
  if (runner.status !== "online") {
    const label = runner.status ? runner.status[0].toUpperCase() + runner.status.slice(1) : "Unknown";
    return { label, variant: "bad" };
  }
  if (runner.draining) return { label: "Draining", variant: "warn" };
  if (!runner.enabled) return { label: "Disabled", variant: "idle" };
  return { label: "Online", variant: "ok" };
}

/** Number of runners the orchestrator currently considers connected. */
export function countOnline(runners: Runner[]): number {
  return runners.filter((runner) => runner.status === "online").length;
}

/**
 * Turns a rejection into the sentence the view shows. `ApiError.message`
 * already carries the problem+json `title: detail`, so it is used verbatim for
 * anything the API answered; 401 and 403 get their own copy because the generic
 * problem titles ("Unauthorized") do not tell an operator what to do next.
 */
export function describeLoadError(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 401) return "Your session has expired. Sign in again to see the runner fleet.";
    if (error.status === 403) return "Your account is not allowed to see the runner fleet.";
    return error.message;
  }
  if (error instanceof Error && error.message) return error.message;
  return "The runner fleet could not be loaded.";
}

/**
 * Loads the fleet and never rejects: a failure becomes an `error` result the
 * view renders, so a fetch failure can never be mistaken for an empty fleet.
 */
export async function loadRunners(api: ApiClient, signal?: AbortSignal): Promise<RunnersResult> {
  try {
    return { kind: "ready", runners: await api.listRunners(signal) };
  } catch (error) {
    return { kind: "error", message: describeLoadError(error) };
  }
}
