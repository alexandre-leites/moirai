// The console's shared vocabulary: workflow statuses, the phase path the thread
// is drawn along, attempt budgets, and the two derivations the console makes
// from the fields the API exposes today.
//
// Statuses and their pill variants come from docs/design/web-console/
// specification.md §3.3; the phase path and its labels from §2.6.
import type { Workflow, WorkflowEvent } from "./api";
import { stripAnsi } from "./ansi";

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

// The Go V1 orchestrator writes exactly thirteen of these
// (orchestrator/internal/server/status.go's Status type is the source of
// truth): offered, preparing, planning, waiting_github_checks, delivering,
// waiting_human, waiting_ai_review, repairing, pipeline_failed and the four
// terminal statuses. `planning` (#351) is a real, wired status: a project that
// opts into requirePlanning sits here while its planner execution runs,
// before ever reaching `preparing`. `delivering` (#359), `waiting_ai_review`
// (#356), `repairing` (#357), `pipeline_failed` (#359) and `waiting_human`
// (the human-approval gate, resolved by SubmitHumanDecision in
// orchestrator/internal/server/management.go) are likewise real, wired
// statuses -- see status.go:16-137 for each one's full semantics; the
// summary that matters here is that all five hold the project lock and are
// active work in progress, exactly like `preparing`/`planning`, so none of
// them belongs in TERMINAL_STATUSES below on that basis alone.
// `pipeline_failed` is the one deliberate exception: status.go documents it
// as "a brief hand-off to the repair-or-block decision", and the console
// treats that hand-off as effectively terminal for the *display* purposes
// TERMINAL_STATUSES serves (the active count, the workflow list's
// active/terminal filter) even though the Go orchestrator's own
// moirai_active_workflows gauge counts it as active until the hand-off
// resolves -- see TERMINAL_STATUSES's comment below. Every other entry this
// table used to carry -- implementing, local_pipeline, ai_review, pushing,
// pr_created, merging, recovering -- named phases of a pipeline the deleted
// internal/workflow package (#247) modelled but the Go orchestrator never
// implemented, and were pruned in #265: statusMeta's fallback below covers a
// status this table does not recognise, which is what every one of those
// would have hit in production anyway.
export const STATUS_META: Record<string, StatusMeta> = {
  offered: { label: "Offered", variant: "idle", pulse: false, phase: "prepare" },
  planning: { label: "Planning", variant: "run", pulse: true, phase: "plan" },
  preparing: { label: "Preparing", variant: "run", pulse: true, phase: "prepare" },
  waiting_github_checks: { label: "Waiting on checks", variant: "wait", pulse: false, phase: "checks" },
  delivering: { label: "Delivering", variant: "run", pulse: true, phase: "pr" },
  waiting_human: { label: "Needs decision", variant: "wait", pulse: false, phase: "human" },
  waiting_ai_review: { label: "In AI review", variant: "run", pulse: true, phase: "review" },
  repairing: { label: "Repairing", variant: "warn", pulse: true, phase: "implement" },
  pipeline_failed: { label: "Pipeline failed", variant: "warn", pulse: false, phase: "pipeline" },
  completed: { label: "Delivered", variant: "ok", pulse: false, phase: "done" },
  blocked: { label: "Blocked", variant: "bad", pulse: false, phase: null },
  failed: { label: "Failed", variant: "bad", pulse: false, phase: null },
  cancelled: { label: "Cancelled", variant: "idle", pulse: false, phase: null },
};

// `pipeline_failed` is included here even though status.go's own
// terminalStatuses (the list its Prometheus active-workflow gauge and
// project-lock bookkeeping use) deliberately excludes it -- a run sitting at
// `pipeline_failed` still holds its project lock and is still active by the
// orchestrator's own accounting, exactly like `delivering`, `waiting_human`,
// `waiting_ai_review` and `repairing`, none of which are listed here either.
// The console's TERMINAL_STATUSES answers a narrower, display-only question
// -- "is this row done enough that a human should stop counting it toward
// the active/needs-attention totals" -- and `pipeline_failed` is a
// deterministic, bounded hand-off (status.go: "a brief hand-off to the
// repair-or-block decision") that resolves to `repairing` or `blocked`
// before an operator can meaningfully act on it as "in flight" work; leaving
// it out of this set is what let pipeline_failed runs show as permanently
// active in the sidebar (#360).
export const TERMINAL_STATUSES = new Set(["completed", "blocked", "failed", "cancelled", "pipeline_failed"]);
/**
 * Terminal runs whose thread is drawn cut rather than finished, and whose
 * event-feed tone reads as a failure rather than a success (see
 * overview.tsx's feedTone). `pipeline_failed` belongs here for the same
 * reason it belongs in TERMINAL_STATUSES above: it names a failed pipeline
 * command, and treating it as a plain "terminal" run without also treating
 * it as a "cut" one would have `feedTone`/`isTerminal` fall through to their
 * completed-run branch and paint a failed run's thread and feed entry green.
 */
export const CUT_STATUSES = new Set(["blocked", "failed", "cancelled", "pipeline_failed"]);
/** The set the "Needs you" triage and the nav's hot count are built from. */
export const NEEDS_ATTENTION_STATUSES = new Set(["waiting_human", "blocked"]);

export function statusMeta(status: string): StatusMeta {
  return STATUS_META[status] ?? { label: status.replaceAll("_", " "), variant: "idle", pulse: false, phase: null };
}

export const isTerminal = (status: string): boolean => TERMINAL_STATUSES.has(status);

// The meters' denominators. The API reports how much of each budget a run has
// spent but not the cap, so the console carries one: these are the caps the
// retired Python orchestrator enforced (RetryBudget), kept as the display
// budget because the Go V1 orchestrator has no retry policy — it neither caps
// these counters nor increments any of them (see `reachedPhase`).
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
 * is only ever written by the phase that owns it. Everything here is therefore
 * evidence of a phase having been entered, never an assumption.
 *
 * The Go V1 orchestrator increments planningAttempts, reviewCycles,
 * ciRepairAttempts and pipelineRepairAttempts, but never implementationAttempts
 * (server.go has no writer for it), so a run that never reached review or a
 * repair cycle -- most of a happy-path `preparing`/`delivering` run's life --
 * has nothing in the counters above to place it past `plan`; the live status
 * below is what carries this for those statuses, exactly as it does for
 * `offered`.
 *
 * Specification task A2 replaces this with gate state derived server-side; this
 * function is the seam that change lands on.
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
 * Until then it lives here.
 *
 * `persistExecutionEvent` (orchestrator/internal/server/server.go) stores the
 * runner's payload flat and unwrapped — there is no envelope to unwrap. The Go
 * vocabulary is `started`/`log`/`progress`/`completed`/`failed`/`cancelled`
 * (runner/internal/control/events.go's `validEventType`) plus
 * `workflow_transition`, `pull_request.created`, `pull_request.merged` and
 * `delivery.failed` (orchestrator/internal/server/{server,delivery}.go). The
 * Python writer's envelope and its recovery event types
 * (`offer_unanswered`/`lease_recovery_offered`/`execution_requeued`) were
 * retired with it in #247 and are not read here: no writer in the current
 * system produces them, so a fallback for that shape would be dead code.
 */
export function executionError(event: WorkflowEvent): string | null {
  if (event.type !== "failed") return null;
  return text(asRecord(event.payload).error) || null;
}

export function describeEvent(event: WorkflowEvent): { text: string; phase: string; warn: boolean } {
  const payload = asRecord(event.payload);

  switch (event.type) {
    case "workflow_transition": {
      // The Go writer's only producer today (controlWorkflow's retry branch)
      // writes `{"reason": ...}` with no `status` field, so the `status`-keyed
      // branch below is dead against real payloads; `reason` is what carries
      // the operator-facing text and is rendered directly when present.
      const status = text(payload.status);
      const reason = text(payload.reason);
      return {
        text: reason || (status ? `Workflow entered ${statusMeta(status).label.toLowerCase()}` : "Workflow state changed"),
        phase: status ? shortPhase(status) : "workflow",
        warn: status === "blocked" || status === "failed",
      };
    }
    case "started":
      // The runner's started payload is just `{"status":"running"}`
      // (control_loop.go's `execute`) — no runner or job identity travels with
      // it, and the events API doesn't surface either alongside the payload
      // (see `WorkflowEvent` in api.ts), so there is nothing more to report.
      return { text: "Agent execution started", phase: "execution", warn: false };
    case "progress":
      return { text: text(payload.message) || "Agent reported progress", phase: "execution", warn: false };
    case "log": {
      // The timeline reads as sentences, so the escape sequences the agent wrote
      // for a terminal are dropped here rather than rendered. The agent log pane
      // is where they are drawn in colour (ui/ansi.tsx).
      const line = logText(event.payload);
      return { text: line === null ? "Agent log output" : stripAnsi(line), phase: "log", warn: false };
    }
    case "completed":
      return { text: "Agent execution completed", phase: "execution", warn: false };
    case "failed": {
      // `terminalPayload` (runner/internal/dispatch/control_loop.go) always
      // sets `exitCode`, but guard `undefined` anyway rather than rendering
      // "exit undefined" if a future writer omits it.
      const exit = payload.exitCode;
      return {
        text: exit === undefined ? "Agent execution failed" : `Agent execution failed (exit ${String(exit)})`,
        phase: "execution",
        warn: true,
      };
    }
    case "cancelled":
      return { text: "Agent execution cancelled", phase: "execution", warn: true };
    case "pull_request.created": {
      // delivery.go's INSERT writes `{"number": "<pr.Number>", "url": "..."}`.
      const number = text(payload.number);
      return { text: number ? `Pull request #${number} opened` : "Pull request opened", phase: "pr", warn: false };
    }
    case "pull_request.merged":
      return { text: "Pull request merged", phase: "merge", warn: false };
    case "plan.recorded": {
      // Written by preparePlanningCompletion (orchestrator/internal/server/
      // server.go) as `{"plan": [...]}` once a planner execution completes;
      // the first entry is always its summary (planFromPayload), the rest its
      // remaining-work steps.
      const plan = Array.isArray(payload.plan) ? (payload.plan as unknown[]).filter((entry) => typeof entry === "string") : [];
      return {
        text: plan.length > 0 ? `Plan ready: ${plan[0] as string}` : "Plan ready",
        phase: "plan",
        warn: false,
      };
    }
    case "delivery.failed": {
      // Written by `terminateWorkflow` with `{"reason": "<cause>"}`.
      const reason = text(payload.reason);
      return { text: reason ? `Delivery failed: ${reason}` : "Delivery failed", phase: "event", warn: true };
    }
    case "human.rejected": {
      // Written by `rejectWorkflow` (orchestrator/internal/server/delivery.go)
      // with `{"reason": "changes requested[: <comment>]"}` when an admin
      // rejects a run at the human-approval gate -- already self-describing,
      // unlike delivery.failed's bare cause, so it is rendered verbatim.
      const reason = text(payload.reason);
      return { text: reason || "Changes requested", phase: "human", warn: true };
    }
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
 * The text a `log` event carried. `EmitLog` (runner/internal/control/events.go)
 * writes `{"message": "<chunk>", "chunkIndex": …, "chunkCount": …}` flat, which
 * the top-level `message` key below covers; `log`/`text`/`line` are kept as
 * fallbacks for any other flat shape a future writer might use.
 */
export function logText(payload: unknown): string | null {
  if (typeof payload === "string") return payload;
  if (!payload || typeof payload !== "object") return null;
  const record = payload as Record<string, unknown>;
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
