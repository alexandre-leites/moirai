// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { OverviewPage } from "./overview";
import { triage } from "./triage";
import { unmountAll } from "./test-dom";
import { event, mountView, project, runner, stubApi, workflow } from "./test-console";

afterEach(unmountAll);

const tileValue = (container: HTMLElement, label: string): string | undefined =>
  Array.from(container.querySelectorAll(".tile"))
    .find((tile) => tile.querySelector(".lab")?.textContent === label)
    ?.querySelector(".val")?.textContent ?? undefined;

describe("triage", () => {
  it("orders decisions before blocked runs before failures with an open PR", () => {
    const items = triage([
      workflow({ id: "wf-blocked", status: "blocked", blockingReason: "non-progress guard" }),
      workflow({ id: "wf-failed", status: "failed", pullRequestUrl: "https://example.test/pull/9" }),
      workflow({ id: "wf-waiting", status: "waiting_human" }),
      workflow({ id: "wf-running", status: "preparing" }),
    ]);
    expect(items.map((item) => item.workflow.id)).toEqual(["wf-waiting", "wf-blocked", "wf-failed"]);
    expect(items[1].detail).toBe("non-progress guard");
  });

  it("leaves out a failure that left no pull request behind", () => {
    expect(triage([workflow({ status: "failed" })])).toHaveLength(0);
  });

  it("says something rather than nothing when no reason was recorded", () => {
    expect(triage([workflow({ status: "blocked", blockingReason: "" })])[0].detail)
      .toBe("No reason was recorded.");
  });
});

describe("OverviewPage", () => {
  const api = () => stubApi({
    listProjects: async () => [project()],
    listWorkflows: async () => [
      workflow({ id: "wf-1", status: "preparing" }),
      workflow({ id: "wf-2", status: "waiting_human", issueExternalId: "#58" }),
      workflow({ id: "wf-3", status: "completed", issueExternalId: "#198" }),
      workflow({ id: "wf-4", status: "blocked", issueExternalId: "#96", blockingReason: "non-progress guard" }),
    ],
    listRunners: async () => [runner(), runner({ id: "r2", name: "loom-03", status: "offline" })],
    listQueue: async () => [],
    listWorkflowEvents: async () => ({ events: [event({ type: "workflow_transition", payload: { status: "preparing" } })] }),
  });

  it("counts the fleet, the queue and the work in flight", async () => {
    const client = api();
    const container = await mountView(<OverviewPage api={client} />, client);

    expect(tileValue(container, "Active workflows")).toBe("2");
    expect(tileValue(container, "Queue depth")).toBe("0");
    expect(tileValue(container, "Runners online")).toContain("1");
    expect(tileValue(container, "Delivered")).toBe("1");
    expect(container.textContent).toContain("1 waiting on you");
    expect(container.textContent).toContain("1 offline");
  });

  it("shows component and reported runner commits", async () => {
    const client = stubApi({
      health: async () => ({ status: "healthy", orchestrator: "reachable", apiVersion: "1234567890abcdef", orchestratorVersion: "abcdef1234567890" }),
      listRunners: async () => [runner({ name: "loom-01", version: "fedcba9876543210" }), runner({ name: "loom-02" })],
    });
    const container = await mountView(<OverviewPage api={client} />, client);
    expect(container.textContent).toContain("System versions");
    expect(container.textContent).toContain("1234567890ab");
    expect(container.textContent).toContain("abcdef123456");
    expect(container.textContent).toContain("fedcba987654");
    expect(container.textContent).not.toContain("Runner: loom-02");
  });

  it("flags a stalled background loop and names it", async () => {
    const client = stubApi({
      schedulerMetrics: async () => ({
        queueDepth: 0,
        activeWorkflows: 0,
        scheduledJobs: 0,
        loops: [
          { name: "issue_sync", healthy: true, lastSuccessAt: "2026-08-03T12:00:00Z" },
          { name: "recovery_sweep", healthy: false, lastError: "gh: rate limited" },
        ],
      }),
    });
    const container = await mountView(<OverviewPage api={client} />, client);

    expect(container.textContent).toContain("Background loops");
    expect(container.textContent).toContain("recovery_sweep");
    expect(container.textContent).toContain("stalled");
  });

  it("reports background loops as healthy when every one is", async () => {
    const client = stubApi({
      schedulerMetrics: async () => ({
        queueDepth: 0,
        activeWorkflows: 0,
        scheduledJobs: 0,
        loops: [{ name: "issue_sync", healthy: true }],
      }),
    });
    const container = await mountView(<OverviewPage api={client} />, client);

    expect(container.textContent).toContain("Background loops");
    expect(container.textContent).toContain("all healthy");
  });

  it("shows Unknown for component versions omitted by a remote build", async () => {
    const client = stubApi();
    const container = await mountView(<OverviewPage api={client} />, client);
    expect(container.textContent).toContain("System versions");
    expect(container.textContent).toContain("Unknown");
  });

  it("lists what needs a human, newest concern first", async () => {
    const client = api();
    const container = await mountView(<OverviewPage api={client} />, client);

    const items = Array.from(container.querySelectorAll(".attn-item"));
    expect(items[0].textContent).toContain("is ready to merge");
    expect(items[1].textContent).toContain("is blocked");
    expect(items[1].textContent).toContain("non-progress guard");
  });

  it("draws a mini thread for every run in flight", async () => {
    const client = api();
    const container = await mountView(<OverviewPage api={client} />, client);
    expect(container.querySelectorAll(".minithread")).toHaveLength(2);
  });

  it("uses the specification's empty copy when nothing needs a decision", async () => {
    const client = stubApi({ listWorkflows: async () => [workflow({ status: "preparing" })] });
    const container = await mountView(<OverviewPage api={client} />, client);
    expect(container.textContent).toContain("The Fates are spinning on their own.");
  });

  it("reports a degraded control plane in the health strip", async () => {
    const client = stubApi({ health: async () => ({ status: "degraded", orchestrator: "unreachable" }) });
    const container = await mountView(<OverviewPage api={client} />, client);
    expect(container.querySelector(".health-strip")?.textContent).toContain("unreachable");
  });

  it("shows a backing-off issue sync as a warning probe", async () => {
    const client = stubApi({
      issueSyncStatus: async () => [{
        projectId: "p1", projectName: "atlas", enabled: true, issueCount: 4, eligibleCount: 1,
        consecutiveFailures: 3, backingOff: true, lastError: "gh: HTTP 502",
      }],
    });
    const container = await mountView(<OverviewPage api={client} />, client);
    expect(container.querySelector(".health-strip")?.textContent).toContain("1 backing off");
  });

  it("surfaces a load failure instead of an empty console", async () => {
    const client = stubApi({ listWorkflows: async () => { throw new Error("orchestrator unreachable"); } });
    const container = await mountView(<OverviewPage api={client} />, client);
    expect(container.textContent).toContain("orchestrator unreachable");
    expect(container.textContent).not.toContain("The Fates are spinning on their own.");
  });
});
