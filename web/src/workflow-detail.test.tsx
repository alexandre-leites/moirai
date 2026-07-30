// @vitest-environment jsdom
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it } from "vitest";
import type { ApiClient, WorkflowDetail, WorkflowEventsPage } from "./api";
import { WorkflowDetailPage } from "./workflow-detail";
import { button, click, deferred, mount, unmountAll } from "./test-dom";

afterEach(unmountAll);

const WORKFLOW_ID = "11111111-2222-3333-4444-555555555555";

function workflow(overrides: Partial<WorkflowDetail> = {}): WorkflowDetail {
  return {
    id: WORKFLOW_ID,
    projectId: "project-1",
    status: "blocked",
    phase: "repairing",
    issueExternalId: "117",
    issueTitle: "Show workflow detail",
    branchName: "agent/117/workflow-detail",
    pullRequestExternalId: "42",
    pullRequestUrl: "https://github.com/acme/repo/pull/42",
    pullRequestState: "open",
    blockingReason: "retry budget exhausted",
    planningAttempts: 1,
    implementationAttempts: 2,
    pipelineRepairAttempts: 3,
    ciRepairAttempts: 4,
    reviewCycles: 5,
    createdAt: "2026-07-30T00:00:00Z",
    updatedAt: "2026-07-30T00:01:00Z",
    ...overrides,
  };
}

function eventsPage(overrides: Partial<WorkflowEventsPage> = {}): WorkflowEventsPage {
  return {
    events: [{
      id: "event-1",
      type: "log",
      createdAt: "2026-07-30T00:01:00Z",
      payload: { payload: { message: "agent output\nsecond line" } },
    }],
    ...overrides,
  };
}

function page(api: ApiClient) {
  return (
    <MemoryRouter initialEntries={[`/workflows/${WORKFLOW_ID}`]}>
      <Routes><Route path="/workflows/:workflowId" element={<WorkflowDetailPage api={api} />} /></Routes>
    </MemoryRouter>
  );
}

function stubApi(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    getWorkflow: async () => workflow(),
    listWorkflowEvents: async () => eventsPage(),
    ...overrides,
  } as ApiClient;
}

describe("WorkflowDetailPage", () => {
  it("renders workflow state, attempts, pull request, and log output", async () => {
    const container = await mount(page(stubApi()));

    expect(container.textContent).toContain("117: Show workflow detail");
    expect(container.textContent).toContain("blocked");
    expect(container.textContent).toContain("repairing");
    expect(container.textContent).toContain("agent/117/workflow-detail");
    expect(container.textContent).toContain("retry budget exhausted");
    expect(container.textContent).toContain("Planning");
    expect(container.textContent).toContain("5");
    expect(container.querySelector('a[href="https://github.com/acme/repo/pull/42"]')).not.toBeNull();
    expect(container.querySelector("pre")?.textContent).toBe("agent output\nsecond line");
  });

  it("says when no pull request or events exist", async () => {
    const container = await mount(page(stubApi({
      getWorkflow: async () => workflow({ pullRequestUrl: undefined }),
      listWorkflowEvents: async () => eventsPage({ events: [] }),
    })));

    expect(container.textContent).toContain("No pull request has been created.");
    expect(container.textContent).toContain("No runner events have been recorded.");
  });

  it("renders timeline events newest first", async () => {
    const container = await mount(page(stubApi({
      listWorkflowEvents: async () => eventsPage({ events: [
        { id: "old", type: "started", createdAt: "2026-07-30T00:00:00Z", payload: {} },
        { id: "new", type: "completed", createdAt: "2026-07-30T00:01:00Z", payload: {} },
      ] }),
    })));

    expect(Array.from(container.querySelectorAll(".workflow-timeline-event")).map((event) => event.textContent)).toEqual([
      expect.stringContaining("completed"),
      expect.stringContaining("started"),
    ]);
  });

  it("loads older events with the returned cursor", async () => {
    const cursors: Array<string | undefined> = [];
    const api = stubApi({
      listWorkflowEvents: async (_id, cursor) => {
        cursors.push(cursor);
        return cursor
          ? eventsPage({ events: [{ id: "event-0", type: "started", createdAt: "2026-07-29T23:59:00Z", payload: {} }] })
          : eventsPage({ nextCursor: "event-1" });
      },
    });
    const container = await mount(page(api));

    await click(button(container, /Load older events/));

    expect(cursors).toEqual([undefined, "event-1"]);
    expect(container.textContent).toContain("started");
    expect(container.querySelector("button")).toBeNull();
  });

  it("renders a load failure instead of an empty workflow", async () => {
    const container = await mount(page(stubApi({
      getWorkflow: async () => {
        throw new Error("orchestrator unavailable");
      },
    })));

    expect(container.querySelector('[role="alert"]')?.textContent).toContain("orchestrator unavailable");
    expect(container.textContent).not.toContain("No runner events have been recorded.");
  });

  it("shows loading before both reads return", async () => {
    const pending = deferred<WorkflowDetail>();
    const container = await mount(page(stubApi({ getWorkflow: () => pending.promise })));

    expect(container.textContent).toContain("Loading workflow...");
    await pending.resolve(workflow());
    expect(container.textContent).not.toContain("Loading workflow...");
  });
});
