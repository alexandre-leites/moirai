// @vitest-environment jsdom
import { act } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { WorkflowsPage, matchesFilter, matchesQuery } from "./workflows";
import { click, unmountAll } from "./test-dom";
import { mountView, stubApi, workflow } from "./test-console";

afterEach(() => {
  vi.unstubAllGlobals();
  return unmountAll();
});

class FakeEventSource {
  static current: FakeEventSource | null = null;
  listeners = new Map<string, (event: Event) => void>();
  constructor() { FakeEventSource.current = this; }
  addEventListener(type: string, listener: (event: Event) => void) { this.listeners.set(type, listener); }
  close() {}
}

const RUNS = [
  workflow({ id: "wf-1", status: "preparing", issueExternalId: "#103", issueTitle: "Close execution requests" }),
  workflow({
    id: "wf-2", status: "waiting_human", issueExternalId: "#58", issueTitle: "Fix flaky token refresh",
    branchName: "agent/58-flaky-token-refresh",
    pullRequestUrl: "https://example.test/pull/112", pullRequestExternalId: "112",
  }),
  workflow({ id: "wf-3", status: "completed", issueExternalId: "#198", issueTitle: "Migrate session storage" }),
  workflow({ id: "wf-4", status: "blocked", issueExternalId: "#96", issueTitle: "Deduplicate label writes" }),
];

const rowsOf = (container: HTMLElement) => Array.from(container.querySelectorAll("tbody tr"));

describe("workflow filters", () => {
  it("splits the statuses the way the specification defines the four chips", () => {
    expect(RUNS.filter((run) => matchesFilter(run, "active")).map((run) => run.id)).toEqual(["wf-1", "wf-2"]);
    expect(RUNS.filter((run) => matchesFilter(run, "needs_you")).map((run) => run.id)).toEqual(["wf-2", "wf-4"]);
    expect(RUNS.filter((run) => matchesFilter(run, "terminal")).map((run) => run.id)).toEqual(["wf-3", "wf-4"]);
    expect(RUNS.filter((run) => matchesFilter(run, "all"))).toHaveLength(4);
  });

  it("searches the fields an operator actually has in hand", () => {
    const run = RUNS[1];
    expect(matchesQuery(run, "helios", "58")).toBe(true);
    expect(matchesQuery(run, "helios", "helios")).toBe(true);
    expect(matchesQuery(run, "helios", "FLAKY")).toBe(true);
    expect(matchesQuery(run, "helios", "agent/58")).toBe(true);
    expect(matchesQuery(run, "helios", "112")).toBe(true);
    expect(matchesQuery(run, "helios", "nothing-matches")).toBe(false);
    expect(matchesQuery(run, "helios", "")).toBe(true);
  });
});

describe("WorkflowsPage", () => {
  it("shows the active runs by default with a thread and an attempts meter", async () => {
    const api = stubApi({ listWorkflows: async () => RUNS });
    const container = await mountView(<WorkflowsPage />, api, { route: "/workflows", path: "/workflows" });

    expect(rowsOf(container)).toHaveLength(2);
    expect(container.textContent).toContain("Close execution requests");
    expect(container.textContent).not.toContain("Migrate session storage");
    expect(container.querySelectorAll(".minithread")).toHaveLength(2);
    expect(container.querySelector(".meter")).not.toBeNull();
  });

  it("updates a workflow from the event stream without a reload", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    const api = stubApi({ listWorkflows: async () => RUNS });
    const container = await mountView(<WorkflowsPage />, api, { route: "/workflows", path: "/workflows" });

    await act(async () => {
      FakeEventSource.current?.listeners.get("workflow")?.(new MessageEvent("workflow", {
        data: JSON.stringify({ type: "workflow", workflow: { ...RUNS[0], issueTitle: "Pushed update" } }),
      }));
    });

    expect(container.textContent).toContain("Pushed update");
  });

  it("reads the filter out of the query string", async () => {
    const api = stubApi({ listWorkflows: async () => RUNS });
    const container = await mountView(<WorkflowsPage />, api, {
      route: "/workflows?filter=terminal", path: "/workflows",
    });
    expect(rowsOf(container)).toHaveLength(2);
    expect(container.textContent).toContain("Migrate session storage");
  });

  it("filters by the search term in the query string", async () => {
    const api = stubApi({ listWorkflows: async () => RUNS });
    const container = await mountView(<WorkflowsPage />, api, {
      route: "/workflows?filter=all&q=flaky", path: "/workflows",
    });
    expect(rowsOf(container)).toHaveLength(1);
    expect(container.textContent).toContain("Fix flaky token refresh");
  });

  it("switches filters when a chip is pressed", async () => {
    const api = stubApi({ listWorkflows: async () => RUNS });
    const container = await mountView(<WorkflowsPage />, api, { route: "/workflows", path: "/workflows" });

    const terminal = Array.from(container.querySelectorAll<HTMLButtonElement>(".chip"))
      .find((chip) => chip.textContent === "Terminal")!;
    await click(terminal);

    expect(terminal.getAttribute("aria-pressed")).toBe("true");
    expect(container.textContent).toContain("Migrate session storage");
  });

  it("makes each row keyboard-activatable and labelled", async () => {
    const api = stubApi({ listWorkflows: async () => RUNS });
    const container = await mountView(<WorkflowsPage />, api, { route: "/workflows", path: "/workflows" });

    const row = container.querySelector<HTMLTableRowElement>("tr.rowlink")!;
    expect(row.getAttribute("tabindex")).toBe("0");
    expect(row.getAttribute("aria-label")).toContain("#103");
  });

  it("stops a pull-request click from bubbling into the row's open handler", async () => {
    const api = stubApi({ listWorkflows: async () => RUNS });
    const container = await mountView(<WorkflowsPage />, api, {
      route: "/workflows?filter=all", path: "/workflows",
    });

    const link = container.querySelector<HTMLAnchorElement>("a[href='https://example.test/pull/112']")!;
    const event = new MouseEvent("click", { bubbles: true, cancelable: true });
    // React delegates from the root, so the row's handler only runs if the
    // event is still propagating by the time React dispatches it. Asserting on
    // the native call is the check: a synthetic stopPropagation forwards to it.
    const stopped = vi.spyOn(event, "stopPropagation");
    await act(async () => { link.dispatchEvent(event); });
    expect(stopped).toHaveBeenCalled();
    expect(container.textContent).toContain("Fix flaky token refresh");
  });

  it("says no workflow matched rather than showing a blank table", async () => {
    const api = stubApi({ listWorkflows: async () => RUNS });
    const container = await mountView(<WorkflowsPage />, api, {
      route: "/workflows?filter=all&q=nothing-matches-this", path: "/workflows",
    });
    expect(container.textContent).toContain("No workflows match this filter");
  });

  it("surfaces a load failure", async () => {
    const api = stubApi({ listWorkflows: async () => { throw new Error("boom"); } });
    const container = await mountView(<WorkflowsPage />, api, { route: "/workflows", path: "/workflows" });
    expect(container.textContent).toContain("boom");
  });
});
