import { describe, expect, it } from "vitest";
import type { Workflow } from "./api";
import {
  ATTEMPT_BUDGETS, CUT_STATUSES, PHASES, TERMINAL_STATUSES, attemptRows, describeEvent, deriveGates, executionError,
  isTerminal, logText, reachedPhase, statusMeta,
} from "./status";
import { workflow } from "./test-console";

const at = (phase: (typeof PHASES)[number]) => PHASES.indexOf(phase);

describe("statusMeta", () => {
  it("maps the domain statuses to the labels the specification names", () => {
    expect(statusMeta("waiting_human").label).toBe("Needs decision");
    expect(statusMeta("waiting_github_checks").label).toBe("Waiting on checks");
    expect(statusMeta("completed")).toMatchObject({ label: "Delivered", variant: "ok", pulse: false });
    expect(statusMeta("preparing")).toMatchObject({ variant: "run", pulse: true });
  });

  it("names a status added server-side rather than dropping it", () => {
    expect(statusMeta("quiescing")).toMatchObject({ label: "quiescing", variant: "idle", phase: null });
  });

  it("covers the four statuses #359/#356/#357 added to the orchestrator (#360)", () => {
    expect(statusMeta("delivering")).toMatchObject({ label: "Delivering", variant: "run", pulse: true, phase: "pr" });
    expect(statusMeta("waiting_ai_review")).toMatchObject({ variant: "run", pulse: true, phase: "review" });
    expect(statusMeta("repairing")).toMatchObject({ label: "Repairing", variant: "warn", pulse: true, phase: "implement" });
    expect(statusMeta("pipeline_failed")).toMatchObject({ variant: "warn", phase: "pipeline" });
  });
});

describe("terminal and cut status sets (#360)", () => {
  it("keeps delivering, waiting_ai_review and repairing out of TERMINAL_STATUSES -- all three hold the project lock", () => {
    expect(isTerminal("delivering")).toBe(false);
    expect(isTerminal("waiting_ai_review")).toBe(false);
    expect(isTerminal("repairing")).toBe(false);
  });

  it("treats pipeline_failed as terminal for display purposes, unlike status.go's own terminalStatuses list", () => {
    expect(TERMINAL_STATUSES.has("pipeline_failed")).toBe(true);
    expect(isTerminal("pipeline_failed")).toBe(true);
  });

  it("cuts a pipeline_failed run's thread instead of drawing it as a plain finished run", () => {
    expect(CUT_STATUSES.has("pipeline_failed")).toBe(true);
  });
});

describe("reachedPhase", () => {
  it("reads progress off the attempt counters, not the status alone", () => {
    expect(reachedPhase(workflow({ planningAttempts: 0, implementationAttempts: 0, status: "preparing" })))
      .toBe(at("prepare"));
    expect(reachedPhase(workflow({ planningAttempts: 1, implementationAttempts: 0, status: "preparing" })))
      .toBe(at("plan"));
    expect(reachedPhase(workflow({ reviewCycles: 2, status: "preparing" }))).toBe(at("review"));
  });

  it("uses the pull request as evidence the run got past the push", () => {
    const run = workflow({ status: "failed", pullRequestUrl: "https://example.test/pull/7", pullRequestState: "open" });
    expect(reachedPhase(run)).toBe(at("checks"));
  });

  it("puts a delivered run at the end of the path", () => {
    expect(reachedPhase(workflow({ status: "completed" }))).toBe(PHASES.length - 1);
  });

  it("never regresses below the phase the live status implies", () => {
    // Counters are all zero, but the run is already waiting on GitHub checks.
    const run = workflow({
      status: "waiting_github_checks", planningAttempts: 0, implementationAttempts: 0, reviewCycles: 0,
    });
    expect(reachedPhase(run)).toBe(at("checks"));
  });

  it("places the four #360 statuses on the phase path from the live status alone", () => {
    expect(reachedPhase(workflow({ status: "delivering" }))).toBe(at("pr"));
    expect(reachedPhase(workflow({ status: "waiting_ai_review" }))).toBe(at("review"));
    expect(reachedPhase(workflow({ status: "repairing" }))).toBe(at("implement"));
    expect(reachedPhase(workflow({ status: "pipeline_failed" }))).toBe(at("pipeline"));
  });
});

describe("deriveGates", () => {
  const stateOf = (run: Workflow, label: string) =>
    deriveGates(run).find((gate) => gate.label === label)?.state;

  it("marks everything before the current phase passed and the rest not reached", () => {
    const run = workflow({ status: "preparing", reviewCycles: 1 });
    expect(stateOf(run, "Plan valid")).toBe("passed");
    expect(stateOf(run, "Local pipeline")).toBe("passed");
    expect(stateOf(run, "AI review")).toBe("pending");
    expect(stateOf(run, "GitHub checks")).toBe("not_reached");
    expect(stateOf(run, "Human approval")).toBe("not_reached");
  });

  it("fails the gate a cut run stopped on", () => {
    const run = workflow({ status: "blocked", pipelineRepairAttempts: 3 });
    expect(stateOf(run, "Plan valid")).toBe("passed");
    expect(stateOf(run, "Local pipeline")).toBe("failed");
    expect(stateOf(run, "AI review")).toBe("not_reached");
  });

  it("passes every gate for a delivered run, including the human one", () => {
    const run = workflow({ status: "completed" });
    expect(deriveGates(run).every((gate) => gate.state === "passed")).toBe(true);
  });

  it("holds the human gate pending while a run waits on a decision", () => {
    const run = workflow({ status: "waiting_human", pullRequestUrl: "https://example.test/pull/7" });
    expect(stateOf(run, "GitHub checks")).toBe("passed");
    expect(stateOf(run, "Human approval")).toBe("pending");
  });

  it("fails the pipeline gate for a pipeline_failed run since it is drawn as a cut thread", () => {
    const run = workflow({ status: "pipeline_failed" });
    expect(stateOf(run, "Local pipeline")).toBe("failed");
    expect(stateOf(run, "AI review")).toBe("not_reached");
  });

  it("marks a bare repairing run's own phase (implement) pending, distinct from a cut run's failed gate", () => {
    const run = workflow({ status: "repairing" });
    expect(stateOf(run, "Local pipeline")).toBe("not_reached");
  });

  it("credits a repairing run's own pipeline attempt once pipelineRepairAttempts records it", () => {
    const run = workflow({ status: "repairing", pipelineRepairAttempts: 1 });
    expect(stateOf(run, "Local pipeline")).toBe("pending");
    expect(stateOf(run, "AI review")).toBe("not_reached");
  });
});

describe("attemptRows", () => {
  it("measures each counter against the orchestrator's retry budget", () => {
    const rows = attemptRows(workflow({ totalAgentExecutions: 10 }));
    expect(rows.map((row) => row.label)).toEqual([
      "Planning", "Implementation", "Pipeline repair", "Review cycles", "CI repair", "Total executions",
    ]);
    const total = rows.at(-1)!;
    expect(total).toMatchObject({ used: 10, budget: ATTEMPT_BUDGETS.totalExecutions, emphasis: true });
  });
});

describe("describeEvent", () => {
  it("names a workflow transition by the status it entered", () => {
    const described = describeEvent({ id: "1", type: "workflow_transition", createdAt: "", payload: { status: "waiting_human" } });
    expect(described.text).toContain("needs decision");
    expect(described.warn).toBe(false);
  });

  it("renders the reason the Go orchestrator's retry transition carries, instead of a generic message", () => {
    const described = describeEvent({
      id: "2", type: "workflow_transition", createdAt: "", payload: { reason: "reopened by manual retry" },
    });
    expect(described.text).toBe("reopened by manual retry");
  });

  it("has no runner identity to report on the runner's started payload", () => {
    // control_loop.go's `execute` emits `{"status":"running"}` and the events
    // API carries no runner/job id alongside the payload.
    const described = describeEvent({ id: "1", type: "started", createdAt: "", payload: { status: "running" } });
    expect(described.text).toBe("Agent execution started");
    expect(described.warn).toBe(false);
  });

  it("reads the flat exitCode terminalPayload writes, not a nested exit_code", () => {
    const described = describeEvent({
      id: "2", type: "failed", createdAt: "",
      payload: { status: "failed", exitCode: 1, error: "agent reported status blocked without a summary" },
    });
    expect(described.text).toBe("Agent execution failed (exit 1)");
    expect(described.warn).toBe(true);
  });

  it("renders the three delivery events delivery.go writes instead of falling through to default", () => {
    expect(describeEvent({ id: "3", type: "pull_request.created", createdAt: "", payload: { number: "42", url: "https://example.test/pull/42" } }).text)
      .toBe("Pull request #42 opened");
    expect(describeEvent({ id: "4", type: "pull_request.merged", createdAt: "", payload: {} }).text)
      .toBe("Pull request merged");
    const failed = describeEvent({ id: "5", type: "delivery.failed", createdAt: "", payload: { reason: "external delivery failed: no runner available" } });
    expect(failed.text).toBe("Delivery failed: external delivery failed: no runner available");
    expect(failed.warn).toBe(true);
  });

  it("renders a rejection at the human-approval gate verbatim, not double-prefixed", () => {
    const rejected = describeEvent({
      id: "5b", type: "human.rejected", createdAt: "", payload: { reason: "changes requested: please add tests" },
    });
    expect(rejected.text).toBe("changes requested: please add tests");
    expect(rejected.warn).toBe(true);
    expect(describeEvent({ id: "5c", type: "human.rejected", createdAt: "", payload: {} }).text)
      .toBe("Changes requested");
  });

  it("reads a log line out of the flat payload EmitLog writes", () => {
    const described = describeEvent({
      id: "6", type: "log", createdAt: "",
      payload: { message: "applying plan step 3/6", chunkIndex: 0, chunkCount: 1 },
    });
    expect(described.text).toBe("applying plan step 3/6");
  });

  it("shows an unknown event type as itself instead of swallowing it", () => {
    expect(describeEvent({ id: "7", type: "some_new_event", createdAt: "", payload: {} }).text)
      .toBe("some new event");
  });
});

describe("executionError", () => {
  it("reads the flat error field terminalPayload writes on a failed event", () => {
    const event = { id: "1", type: "failed", createdAt: "", payload: { status: "failed", exitCode: 1, error: "boom" } };
    expect(executionError(event)).toBe("boom");
  });

  it("returns null for non-failed events and for a failed event without an error", () => {
    expect(executionError({ id: "2", type: "completed", createdAt: "", payload: { status: "completed" } })).toBeNull();
    expect(executionError({ id: "3", type: "failed", createdAt: "", payload: { status: "failed", exitCode: 1 } })).toBeNull();
  });
});

describe("logText", () => {
  it("reads the flat message key EmitLog writes", () => {
    expect(logText({ message: "applying plan step 3/6", chunkIndex: 0, chunkCount: 1 })).toBe("applying plan step 3/6");
  });

  it("returns null when there is no text anywhere in the payload", () => {
    expect(logText({ status: "running" })).toBeNull();
    expect(logText(null)).toBeNull();
  });
});
