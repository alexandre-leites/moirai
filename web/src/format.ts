// Formatting shared by every view. Timestamps render as a relative age with the
// absolute time in `title` (specification.md §5).

const MINUTE = 60;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * A compact age such as "12s", "4m", "3h 20m", "2d". Returns null when the
 * timestamp is missing or unparseable, so callers can show their own fallback
 * instead of the string "Invalid Date".
 */
export function age(timestamp: string | undefined | null, now: number = Date.now()): string | null {
  if (!timestamp) return null;
  const parsed = Date.parse(timestamp);
  if (Number.isNaN(parsed)) return null;

  const seconds = Math.max(0, Math.round((now - parsed) / 1000));
  if (seconds < MINUTE) return `${seconds}s`;
  if (seconds < HOUR) return `${Math.floor(seconds / MINUTE)}m`;
  if (seconds < DAY) {
    const hours = Math.floor(seconds / HOUR);
    const minutes = Math.floor((seconds % HOUR) / MINUTE);
    return minutes === 0 ? `${hours}h` : `${hours}h ${minutes}m`;
  }
  const days = Math.floor(seconds / DAY);
  const hours = Math.floor((seconds % DAY) / HOUR);
  return hours === 0 ? `${days}d` : `${days}d ${hours}h`;
}

/** "12s ago", or the fallback when there is no usable timestamp. */
export function ageAgo(timestamp: string | undefined | null, fallback = "never", now: number = Date.now()): string {
  const value = age(timestamp, now);
  return value === null ? fallback : `${value} ago`;
}

/** The absolute time, for the `title` attribute beside a relative age. */
export function absolute(timestamp: string | undefined | null): string | undefined {
  if (!timestamp) return undefined;
  const parsed = new Date(timestamp);
  return Number.isNaN(parsed.getTime()) ? timestamp : parsed.toLocaleString();
}

/** Clock time for event rows, which are always same-day-ish and read better short. */
export function clock(timestamp: string): string {
  const parsed = new Date(timestamp);
  if (Number.isNaN(parsed.getTime())) return timestamp;
  return parsed.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
}

/** Short clock without seconds, for the overview feed's fixed-width time column. */
export function shortClock(timestamp: string): string {
  const parsed = new Date(timestamp);
  if (Number.isNaN(parsed.getTime())) return timestamp;
  return parsed.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
}

/** "1 item" / "3 items", so callers stop hand-rolling the plural. */
export function plural(count: number, singular: string, pluralForm = `${singular}s`): string {
  return `${count} ${count === 1 ? singular : pluralForm}`;
}

/**
 * The queue's `blockedReason` enum in operator English. The reasons come from
 * the orchestrator's scheduler (domain/scheduling.py); an unrecognized value is
 * passed through rather than swallowed, so a new reason shows up as itself.
 */
const HOLD_REASONS: Record<string, string> = {
  "": "Next to schedule",
  none: "Next to schedule",
  project_disabled: "Project is paused — scheduling is disabled",
  project_locked: "Project busy — one workflow per project",
  project_circuit_open: "Project circuit open — waiting for the cooldown probe",
  provider_circuit_open: "Code-host circuit open — waiting for the cooldown probe",
  no_matching_runner: "No runner matches this project's required labels",
};

export function holdReason(reason: string): string {
  return HOLD_REASONS[reason] ?? reason.replaceAll("_", " ");
}
