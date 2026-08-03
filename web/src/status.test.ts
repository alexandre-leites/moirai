import { describe, expect, it } from "vitest";
import type { Workflow } from "./api";
import {
  ATTEMPT_BUDGETS, PHASES, attemptRows, describeEvent, deriveGates, logText, reachedPhase, statusMeta,
} from "./status";
import { workflow } from "./test-console";

const at = (phase: (typeof PHASES)[number]) => PHASES.indexOf(phase);

describe("statusMeta", () => {
  it("maps the domain statuses to the labels the specification names", () => {
    expect(statusMeta("waiting_human").label).toBe("Needs decision");
    expect(statusMeta("local_pipeline").label).toBe("Pipeline");
    expect(statusMeta("completed")).toMatchObject({ label: "Delivered", variant: "ok", pulse: false });
    expect(statusMeta("repairing")).toMatchObject({ variant: "warn", pulse: true });
  });

  it("names a status added server-side rather than dropping it", () => {
    expect(statusMeta("quiescing")).toMatchObject({ label: "quiescing", variant: "idle", phase: null });
  });
});

describe("reachedPhase", () => {
  it("reads progress off the attempt counters, not the status alone", () => {
    expect(reachedPhase(workflow({ planningAttempts: 0, implementationAttempts: 0, status: "preparing" })))
      .toBe(at("prepare"));
    expect(reachedPhase(workflow({ planningAttempts: 1, implementationAttempts: 0, status: "planning" })))
      .toBe(at("plan"));
    expect(reachedPhase(workflow({ reviewCycles: 2, status: "ai_review" }))).toBe(at("review"));
  });

  it("uses the pull request as evidence the run got past the push", () => {
    const run = workflow({ status: "failed", pullRequestUrl: "https://example.test/pull/7", pullRequestState: "open" });
    expect(reachedPhase(run)).toBe(at("checks"));
  });

  it("puts a delivered run at the end of the path", () => {
    expect(reachedPhase(workflow({ status: "completed" }))).toBe(PHASES.length - 1);
  });

  it("never regresses below the phase the live status implies", () => {
    // Counters are all zero, but the run says it is merging.
    const run = workflow({
      status: "merging", planningAttempts: 0, implementationAttempts: 0, reviewCycles: 0,
    });
    expect(reachedPhase(run)).toBe(at("merge"));
  });
});

describe("deriveGates", () => {
  const stateOf = (run: Workflow, label: string) =>
    deriveGates(run).find((gate) => gate.label === label)?.state;

  it("marks everything before the current phase passed and the rest not reached", () => {
    const run = workflow({ status: "ai_review", reviewCycles: 1 });
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

  it("flags the recovery and failure events as warnings", () => {
    expect(describeEvent({ id: "1", type: "offer_unanswered", createdAt: "", payload: { reason: "ttl" } }).warn).toBe(true);
    expect(describeEvent({ id: "2", type: "execution_requeued", createdAt: "", payload: { role: "developer", attempt: 2 } }).warn).toBe(true);
    expect(describeEvent({ id: "3", type: "failed", createdAt: "", payload: { payload: { exit_code: 1 } } }))
      .toMatchObject({ warn: true });
  });

  it("reads a log line out of the envelope the orchestrator wraps runner events in", () => {
    const described = describeEvent({
      id: "4", type: "log", createdAt: "",
      payload: { job_id: "j1", runner_id: "loom-01", payload: { message: "applying plan step 3/6" } },
    });
    expect(described.text).toBe("applying plan step 3/6");
  });

  it("shows an unknown event type as itself instead of swallowing it", () => {
    expect(describeEvent({ id: "5", type: "some_new_event", createdAt: "", payload: {} }).text)
      .toBe("some new event");
  });
});

describe("logText", () => {
  it("returns null when there is no text anywhere in the payload", () => {
    expect(logText({ job_id: "j1", payload: { exit_code: 0 } })).toBeNull();
    expect(logText(null)).toBeNull();
  });
});
