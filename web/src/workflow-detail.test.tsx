// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ApiClient } from "./api";
import { ApiError } from "./api";
import { WorkflowDetailPage } from "./workflow-detail";
import { button, buttons, click, textarea, typeInto, unmountAll } from "./test-dom";
import { NOW, VIEWER, event, mountView, project, stubApi, workflow } from "./test-console";

afterEach(unmountAll);

const AT = { route: "/workflows/wf-1", path: "/workflows/:workflowId" };

const mountDetail = (api: ApiClient) => mountView(<WorkflowDetailPage api={api} />, api, AT);

describe("WorkflowDetailPage", () => {
  it("heads the view with the project, issue and status, and draws the full thread", async () => {
    const api = stubApi({
      listProjects: async () => [project({ id: "project-1", name: "moirai" })],
      getWorkflow: async () => workflow({ status: "ai_review", reviewCycles: 1 }),
    });
    const container = await mountDetail(api);

    expect(container.querySelector("h1")?.textContent).toBe("moirai #103");
    expect(container.textContent).toContain("AI review");
    expect(container.querySelector(".threadline")).not.toBeNull();
    expect(container.textContent).toContain("spun");
  });

  it("shows the blocking reason in a banner when a run is blocked", async () => {
    const api = stubApi({
      getWorkflow: async () => workflow({ status: "blocked", blockingReason: "Non-progress: 4 identical outcomes" }),
    });
    const container = await mountDetail(api);

    const banner = container.querySelector(".banner")!;
    expect(banner.textContent).toContain("Blocked.");
    expect(banner.textContent).toContain("Non-progress: 4 identical outcomes");
    expect(button(container, /Retry with fresh context/)).toBeTruthy();
  });

  it("shows a failure banner with a retry for a failed run", async () => {
    const api = stubApi({ getWorkflow: async () => workflow({ status: "failed", blockingReason: "Offer expired" }) });
    const container = await mountDetail(api);
    expect(container.querySelector(".banner")?.textContent).toContain("Failed.");
    expect(button(container, /Retry workflow/)).toBeTruthy();
  });

  it("shows a cancelled banner with a retry affordance, since cancelled runs are retryable server-side", async () => {
    const api = stubApi({ getWorkflow: async () => workflow({ status: "cancelled" }) });
    const container = await mountDetail(api);
    expect(container.querySelector(".banner")?.textContent).toContain("Cancelled.");
    expect(button(container, /Retry workflow/)).toBeTruthy();
  });

  it("does not claim retry carries prior-failure context, since V1 does not carry any", async () => {
    const api = stubApi({ getWorkflow: async () => workflow({ status: "failed", blockingReason: "Offer expired" }) });
    const container = await mountDetail(api);
    await click(button(container, /Retry workflow/));
    expect(document.querySelector("#toast")?.textContent).not.toContain("prior-failure context");
    expect(document.querySelector("#toast")?.textContent).toContain("reopened");
  });

  it("posts the decision and its comment when the gate is approved", async () => {
    const submitWorkflowDecision = vi.fn(async () => workflow({ status: "merging" }));
    const api = stubApi({
      getWorkflow: async () => workflow({ status: "waiting_human", pullRequestUrl: "https://example.test/pull/112", pullRequestExternalId: "112" }),
      submitWorkflowDecision,
    });
    const container = await mountDetail(api);

    expect(container.textContent).toContain("Your decision gates the merge");
    await typeInto(textarea(container, "Decision comment"), "looks right, merging");
    await click(button(container, /Approve & merge/));

    expect(submitWorkflowDecision).toHaveBeenCalledWith("wf-1", "approved", "looks right, merging");
    expect(container.textContent).toContain("Approved");
  });

  it("tells a viewer the gate is not theirs to open", async () => {
    const api = stubApi({
      me: async () => VIEWER,
      getWorkflow: async () => workflow({ status: "waiting_human" }),
    });
    const container = await mountDetail(api);

    expect(container.textContent).toContain("An admin has to approve the merge");
    expect(buttons(container, /Approve/)).toHaveLength(0);
  });

  it("reports a 403 as a permission problem rather than signing the user out", async () => {
    const api = stubApi({
      getWorkflow: async () => workflow({ status: "waiting_human" }),
      submitWorkflowDecision: async () => { throw new ApiError(403, "Forbidden"); },
    });
    const container = await mountDetail(api);
    await click(button(container, /Approve & merge/));
    expect(container.textContent).toContain("You need the admin role for that");
  });

  it("requires a reason before a run can be blocked, and confirms first", async () => {
    const blockWorkflow = vi.fn(async () => workflow({ status: "blocked" }));
    const api = stubApi({ getWorkflow: async () => workflow({ status: "implementing" }), blockWorkflow });
    const container = await mountDetail(api);

    const block = button(container, /Block & hold issue/);
    expect(block.disabled).toBe(true);

    await typeInto(textarea(container, "Reason"), "holding for a product decision");
    await click(button(container, /Block & hold issue/));

    // The dialog names the consequence before anything is sent.
    expect(document.querySelector(".modal")?.textContent).toContain("agent:blocked");
    expect(blockWorkflow).not.toHaveBeenCalled();
    await click(button(document.body, /^Block workflow$/));
    expect(blockWorkflow).toHaveBeenCalledWith("wf-1", "holding for a product decision");
  });

  it("hides the controls on a terminal run and from a viewer", async () => {
    const terminal = stubApi({ getWorkflow: async () => workflow({ status: "completed" }) });
    const terminalView = await mountDetail(terminal);
    expect(buttons(terminalView, /Cancel workflow/)).toHaveLength(0);

    const viewer = stubApi({ me: async () => VIEWER, getWorkflow: async () => workflow({ status: "implementing" }) });
    const viewerView = await mountDetail(viewer);
    expect(buttons(viewerView, /Cancel workflow/)).toHaveLength(0);
  });

  it("renders events as sentences and the agent's log lines as text", async () => {
    const api = stubApi({
      listWorkflowEvents: async () => ({
        events: [
          event({ id: "12", type: "log", payload: { payload: { message: "applying plan step 3/6" } }, createdAt: NOW }),
          event({ id: "11", type: "execution_requeued", payload: { role: "developer", attempt: 2 }, createdAt: NOW }),
        ],
      }),
    });
    const container = await mountDetail(api);

    expect(container.textContent).toContain("Execution lost and requeued");
    expect(container.textContent).not.toContain("Execution errors");
    expect(container.querySelector(".evt.warn")).not.toBeNull();
    expect(container.querySelector(".logline")?.textContent).toContain("applying plan step 3/6");
  });

  it("shows execution errors when a runner reports one", async () => {
    const api = stubApi({
      listWorkflowEvents: async () => ({ events: [event({ type: "failed", payload: { payload: { error: "GITHUB_TOKEN is not configured" } } })] }),
    });
    const container = await mountDetail(api);
    expect(container.textContent).toContain("Execution errors");
    expect(container.textContent).toContain("GITHUB_TOKEN is not configured");
  });

  it("draws the agent's colour instead of printing its escape sequences", async () => {
    // Verbatim from workflow 9d1dddb2: opencode formats for a TTY, so every log
    // event arrives with SGR sequences in it. Rendered as text they showed up as
    // `[0m` litter in both cards.
    const line = "\u001b[0m\u001b[0mGrep \"agent:ready\"\u001b[90m 79 matches\u001b[0m";
    const api = stubApi({
      listWorkflowEvents: async () => ({
        events: [event({ id: "12", type: "log", payload: { payload: { message: line } }, createdAt: NOW })],
      }),
    });
    const container = await mountDetail(api);

    const log = container.querySelector(".logline")!;
    expect(log.textContent).toContain('Grep "agent:ready" 79 matches');
    expect(log.textContent).not.toContain("[0m");
    expect(log.querySelector("span")?.getAttribute("style")).toContain("var(--ansi-8)");

    // The timeline reads as sentences, so it takes the same text without colour.
    const timeline = container.querySelector(".evt .tx")!;
    expect(timeline.textContent).toBe('Grep "agent:ready" 79 matches');
    expect(timeline.querySelector("span")).toBeNull();
  });

  it("pages older events once and then stops offering to", async () => {
    // The last page comes back without a cursor. Falling back to the live page's
    // cursor there would re-request the same rows forever.
    const listWorkflowEvents = vi.fn(async (_id: string, options?: { cursor?: string }) =>
      options?.cursor
        ? { events: [event({ id: "1", type: "started", createdAt: "2026-08-01T10:00:00Z" })] }
        : { events: [event({ id: "9" })], nextCursor: "9" }
    );
    const api = stubApi({ listWorkflowEvents });
    const container = await mountDetail(api);

    await click(button(container, /Load older events/));
    expect(container.textContent).toContain("Agent execution started");
    expect(buttons(container, /Load older events/)).toHaveLength(0);
  });

  it("reads the agent log out of the same page the timeline uses", async () => {
    const listWorkflowEvents = vi.fn(async () => ({
      events: [event({ id: "3", type: "log", payload: { payload: { message: "compiling" } } })],
    }));
    const api = stubApi({ listWorkflowEvents });
    const container = await mountDetail(api);

    expect(container.querySelector(".logline")?.textContent).toContain("compiling");
    // One request for both cards, not one each.
    expect(listWorkflowEvents).toHaveBeenCalledTimes(1);
  });

  it("shows gates and the six attempt meters", async () => {
    const api = stubApi({ getWorkflow: async () => workflow({ status: "ai_review", reviewCycles: 1 }) });
    const container = await mountDetail(api);

    expect(container.textContent).toContain("Plan valid");
    expect(container.textContent).toContain("Human approval");
    expect(container.textContent).toContain("not reached");
    expect(container.querySelectorAll(".meter")).toHaveLength(6);
  });

  it("surfaces a load failure with a retry", async () => {
    const api = stubApi({ getWorkflow: async () => { throw new Error("workflow run is unknown"); } });
    const container = await mountDetail(api);
    expect(container.textContent).toContain("workflow run is unknown");
    expect(button(container, /Retry/)).toBeTruthy();
  });
});
