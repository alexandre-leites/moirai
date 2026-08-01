// The console's shared vocabulary: workflow statuses, the phase path the thread
// is drawn along, attempt budgets, and the two derivations the console makes
// from the fields the API exposes today.
//
// Statuses and their pill variants come from docs/design/web-console/
// specification.md §3.3; the phase path and its labels from §2.6.
import type { Workflow, WorkflowEvent } from "./api";

export const PHASES = [
  "prepare", "plan", "implement", "pipeline", "review",
  "push", "pr", "checks", "human", "merge", "done",
] as const;

export type Phase = (typeof PHASES)[number];

export const PHASE_LABEL: Record<Phase, string> = {
  prepare: "Prepare",
  plan: "Plan",
  implement: "Implement",
  pipeline: "Pipeline",
  review: "AI review",
  push: "Push",
  pr: "Open PR",
  checks: "GitHub checks",
  human: "Human gate",
  merge: "Merge",
  done: "Complete",
};

export type PillVariant = "run" | "ok" | "bad" | "warn" | "wait" | "idle";

export type StatusMeta = {
  label: string;
  variant: PillVariant;
  /** Whether the pill's dot pulses — reserved for work actively in flight. */
  pulse: boolean;
  /** Where this status sits on the phase path, or null for terminal-bad states. */
  phase: Phase | null;
};

export const STATUS_META: Record<string, StatusMeta> = {
  offered: { label: "Offered", variant: "idle", pulse: false, phase: "prepare" },
  preparing: { label: "Preparing", variant: "run", pulse: true, phase: "prepare" },
  planning: { label: "Planning", variant: "run", pulse: true, phase: "plan" },
  implementing: { label: "Implementing", variant: "run", pulse: true, phase: "implement" },
  local_pipeline: { label: "Pipeline", variant: "run", pulse: true, phase: "pipeline" },
  repairing: { label: "Repairing", variant: "warn", pulse: true, phase: "pipeline" },
  ai_review: { label: "AI review", variant: "run", pulse: true, phase: "review" },
  pushing: { label: "Pushing", variant: "run", pulse: true, phase: "push" },
  pr_created: { label: "PR open", variant: "wait", pulse: false, phase: "pr" },
  waiting_github_checks: { label: "Waiting on checks", variant: "wait", pulse: false, phase: "checks" },
  waiting_human: { label: "Needs decision", variant: "wait", pulse: false, phase: "human" },
  merging: { label: "Merging", variant: "run", pulse: true, phase: "merge" },
  recovering: { label: "Recovering", variant: "warn", pulse: true, phase: "implement" },
  completed: { label: "Delivered", variant: "ok", pulse: false, phase: "done" },
  blocked: { label: "Blocked", variant: "bad", pulse: false, phase: null },
  failed: { label: "Failed", variant: "bad", pulse: false, phase: null },
  cancelled: { label: "Cancelled", variant: "idle", pulse: false, phase: null },
};

export const TERMINAL_STATUSES = new Set(["completed", "blocked", "failed", "cancelled"]);
/** Terminal runs whose thread is drawn cut rather than finished. */
export const CUT_STATUSES = new Set(["blocked", "failed", "cancelled"]);
/** The set the "Needs you" triage and the nav's hot count are built from. */
export const NEEDS_ATTENTION_STATUSES = new Set(["waiting_human", "blocked"]);

export function statusMeta(status: string): StatusMeta {
  return STATUS_META[status] ?? { label: status.replaceAll("_", " "), variant: "idle", pulse: false, phase: null };
}

export const isTerminal = (status: string): boolean => TERMINAL_STATUSES.has(status);

// Mirrors RetryBudget in orchestrator/src/moirai/workflows/policy.py. The API
// reports how much of each budget a run has spent but not the cap, which is a
// deployment-wide constant rather than per-run state.
export const ATTEMPT_BUDGETS = {
  planning: 2,
  implementation: 3,
  pipelineRepair: 3,
  reviewCycles: 3,
  ciRepair: 3,
  totalExecutions: 10,
} as const;

export type AttemptRow = { label: string; used: number; budget: number; emphasis?: boolean };

export function attemptRows(workflow: Workflow): AttemptRow[] {
  return [
    { label: "Planning", used: workflow.planningAttempts, budget: ATTEMPT_BUDGETS.planning },
    { label: "Implementation", used: workflow.implementationAttempts, budget: ATTEMPT_BUDGETS.implementation },
    { label: "Pipeline repair", used: workflow.pipelineRepairAttempts, budget: ATTEMPT_BUDGETS.pipelineRepair },
    { label: "Review cycles", used: workflow.reviewCycles, budget: ATTEMPT_BUDGETS.reviewCycles },
    { label: "CI repair", used: workflow.ciRepairAttempts, budget: ATTEMPT_BUDGETS.ciRepair },
    { label: "Total executions", used: workflow.totalAgentExecutions, budget: ATTEMPT_BUDGETS.totalExecutions, emphasis: true },
  ];
}

const phaseIndex = (phase: Phase): number => PHASES.indexOf(phase);

/**
 * How far along the phase path a run demonstrably got.
 *
 * `workflow_runs.current_phase` tracks the status column and is overwritten with
 * `blocked`/`failed`/`cancelled` when a run ends badly, so it cannot say where a
 * terminal run stopped. The attempt counters and the pull request can: each one
 * is only ever incremented by the node that owns that phase. Everything here is
 * therefore evidence of a phase having been entered, never an assumption.
 *
 * Specification task A2 replaces this with gate state derived server-side from
 * the checkpoint; this function is the seam that change lands on.
 */
export function reachedPhase(workflow: Workflow): number {
  let reached = 0;
  if (workflow.planningAttempts > 0) reached = Math.max(reached, phaseIndex("plan"));
  if (workflow.implementationAttempts > 0) reached = Math.max(reached, phaseIndex("implement"));
  if (workflow.pipelineRepairAttempts > 0) reached = Math.max(reached, phaseIndex("pipeline"));
  if (workflow.reviewCycles > 0) reached = Math.max(reached, phaseIndex("review"));
  if (workflow.pullRequestUrl) reached = Math.max(reached, phaseIndex("pr"));
  if (workflow.ciRepairAttempts > 0 || workflow.pullRequestState) reached = Math.max(reached, phaseIndex("checks"));
  if (workflow.status === "completed") return phaseIndex("done");

  const live = statusMeta(workflow.status).phase;
  if (live) reached = Math.max(reached, phaseIndex(live));
  return reached;
}

export type GateState = "passed" | "failed" | "pending" | "not_reached";

export type Gate = { label: string; state: GateState; pendingLabel: string };

/** The five delivery gates, derived from `reachedPhase` (see its caveat). */
export function deriveGates(workflow: Workflow): Gate[] {
  const reached = reachedPhase(workflow);
  const cut = CUT_STATUSES.has(workflow.status);

  const gate = (label: string, phase: Phase, pendingLabel: string): Gate => {
    const index = phaseIndex(phase);
    if (reached > index) return { label, state: "passed", pendingLabel };
    if (reached < index) return { label, state: "not_reached", pendingLabel };
    // The run stopped exactly here: a cut run failed this gate, a live one is
    // still working on it.
    return { label, state: cut ? "failed" : "pending", pendingLabel };
  };

  return [
    gate("Plan valid", "plan", "planning"),
    gate("Local pipeline", "pipeline", "running"),
    gate("AI review", "review", "reviewing"),
    gate("GitHub checks", "checks", "pending"),
    workflow.status === "completed"
      ? { label: "Human approval", state: "passed", pendingLabel: "waiting" }
      : gate("Human approval", "human", "waiting"),
  ];
}

export const GATE_LABEL: Record<GateState, string> = {
  passed: "✓ passed",
  failed: "✗ failed",
  pending: "in progress",
  not_reached: "not reached",
};

// --- Events ---------------------------------------------------------------

/**
 * One human-readable line per workflow event.
 *
 * The events API returns the raw `event_type` and payload; specification task A3
 * moves this rendering to the orchestrator so every client agrees on the wording.
 * Until then it lives here, and every branch is driven by a field the writer in
 * orchestrator/src/moirai/persistence/control_plane.py actually stores.
 */
export function describeEvent(event: WorkflowEvent): { text: string; phase: string; warn: boolean } {
  const payload = asRecord(event.payload);
  const inner = asRecord(payload.payload);

  switch (event.type) {
    case "workflow_transition": {
      const status = text(payload.status);
      return {
        text: status ? `Workflow entered ${statusMeta(status).label.toLowerCase()}` : "Workflow state changed",
        phase: status ? shortPhase(status) : "workflow",
        warn: status === "blocked" || status === "failed",
      };
    }
    case "started":
      return { text: `Agent execution started on runner ${text(payload.runner_id) || "unknown"}`, phase: "execution", warn: false };
    case "progress":
      return { text: text(inner.message) || "Agent reported progress", phase: "execution", warn: false };
    case "log":
      return { text: logText(event.payload) ?? "Agent log output", phase: "log", warn: false };
    case "completed":
      return { text: "Agent execution completed", phase: "execution", warn: false };
    case "failed": {
      const exit = inner.exit_code ?? payload.exit_code;
      return {
        text: exit === undefined ? "Agent execution failed" : `Agent execution failed (exit ${String(exit)})`,
        phase: "execution",
        warn: true,
      };
    }
    case "cancelled":
      return { text: "Agent execution cancelled", phase: "execution", warn: true };
    case "offer_unanswered":
      return { text: `Job offer went unanswered — ${text(payload.reason) || "no runner accepted it"}`, phase: "schedule", warn: true };
    case "lease_recovery_offered":
      return { text: `Lease expired on runner ${text(payload.runner_id) || "unknown"} — the job was re-offered`, phase: "recovery", warn: true };
    case "execution_requeued":
      return { text: `Execution lost and requeued (${text(payload.role) || "agent"}, attempt ${String(payload.attempt ?? "?")})`, phase: "recovery", warn: true };
    default:
      return { text: event.type.replaceAll("_", " "), phase: "event", warn: false };
  }
}

/** The status vocabulary is long; the events column shows a compact stem. */
function shortPhase(status: string): string {
  const phase = statusMeta(status).phase;
  return phase ?? status;
}

/**
 * The text a `log` event carried, dug out of the envelope the orchestrator wraps
 * runner events in (`{job_id, runner_id, …, payload: {…}}`).
 */
export function logText(payload: unknown): string | null {
  if (typeof payload === "string") return payload;
  if (!payload || typeof payload !== "object") return null;
  const record = payload as Record<string, unknown>;
  const nested = record.payload;
  if (nested && typeof nested === "object") {
    const value = logText(nested);
    if (value) return value;
  }
  for (const key of ["message", "log", "text", "line"]) {
    if (typeof record[key] === "string") return record[key] as string;
  }
  return null;
}

function asRecord(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" ? (value as Record<string, unknown>) : {};
}

function text(value: unknown): string {
  return typeof value === "string" ? value : "";
}
