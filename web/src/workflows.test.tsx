// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { ApiError, type ApiClient, type Workflow } from "./api";
import { WorkflowsPage } from "./workflows";
import { button, buttons, click, deferred, mount, unmountAll } from "./test-dom";

afterEach(unmountAll);

type Decision = { id: string; decision: "approved" | "changes_requested" };

const WAITING_ID = "11111111-2222-3333-4444-555555555555";
const RUNNING_ID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";

function workflow(overrides: Partial<Workflow> = {}): Workflow {
  return {
    id: WAITING_ID,
    projectId: "billing",
    status: "waiting_human",
    phase: "review",
    ...overrides,
  };
}

type Stub = {
  listWorkflows?: ApiClient["listWorkflows"];
  listProjects?: ApiClient["listProjects"];
  submitWorkflowDecision?: ApiClient["submitWorkflowDecision"];
};

function stubApi(stub: Stub) {
  const decisions: Decision[] = [];
  const api = {
    listWorkflows: stub.listWorkflows ?? (async () => []),
    listProjects: stub.listProjects ?? (async () => []),
    submitWorkflowDecision:
      stub.submitWorkflowDecision ??
      (async (id: string, decision: "approved" | "changes_requested") => {
        decisions.push({ id, decision });
        return workflow({ id, status: "running", phase: "implement" });
      }),
  } as unknown as ApiClient;
  return { api, decisions };
}

/** The `<tr>` in the table body whose shortened id matches `id`. */
function page(api: ApiClient) {
  return <MemoryRouter><WorkflowsPage api={api} /></MemoryRouter>;
}

function row(container: HTMLElement, id: string): HTMLTableRowElement {
  const found = Array.from(container.querySelectorAll("tbody tr")).find(
    (candidate) => candidate.querySelector("td")?.textContent === id.slice(0, 12)
  );
  if (!found) throw new Error(`no row for "${id}" in:\n${container.innerHTML}`);
  return found as HTMLTableRowElement;
}

describe("WorkflowsPage loading and listing", () => {
  it("shows a loading line before the first response, not an empty table", async () => {
    const pending = deferred<Workflow[]>();
    const { api } = stubApi({ listWorkflows: () => pending.promise });
    const container = await mount(page(api));

    expect(container.textContent).toContain("Loading workflows...");
    expect(container.querySelector("table")).toBeNull();
    expect(container.textContent).not.toContain("No active workflows");

    await pending.resolve([]);
    expect(container.textContent).not.toContain("Loading workflows...");
  });

  it("shows an explicit empty state when nothing is running", async () => {
    const { api } = stubApi({ listWorkflows: async () => [] });
    const container = await mount(page(api));

    expect(container.textContent).toContain("No active workflows");
    expect(container.querySelector("table")).toBeNull();
  });

  it("renders every column the operator needs, one row per workflow", async () => {
    const { api } = stubApi({
      listWorkflows: async () => [
        workflow(),
        workflow({ id: RUNNING_ID, projectId: "ledger", status: "running", phase: "implement" }),
      ],
    });
    const container = await mount(page(api));

    expect(Array.from(container.querySelectorAll("th")).map((th) => th.textContent)).toEqual([
      "ID",
      "Project",
      "Status",
      "Phase",
      "Approval",
      "Controls",
    ]);
    expect(container.querySelectorAll("tbody tr")).toHaveLength(2);
    expect(row(container, RUNNING_ID).textContent).toContain("ledger");
    expect(row(container, RUNNING_ID).textContent).toContain("running");
    expect(row(container, RUNNING_ID).textContent).toContain("implement");
  });

  it("shortens the workflow id rather than printing the whole UUID", async () => {
    const { api } = stubApi({ listWorkflows: async () => [workflow()] });
    const container = await mount(page(api));

    expect(container.querySelector("td.mono")?.textContent).toBe("11111111-222");
    expect(container.textContent).not.toContain(WAITING_ID);
  });

  it("renders a failure instead of an empty state when loading fails", async () => {
    const { api } = stubApi({
      listWorkflows: async () => {
        throw new ApiError(503, "orchestrator unavailable");
      },
    });
    const container = await mount(page(api));

    expect(container.querySelector('[role="alert"]')?.textContent).toContain("orchestrator unavailable");
    expect(container.textContent).not.toContain("No active workflows");
  });

  it("links rows to detail pages and uses project names", async () => {
    const { api } = stubApi({
      listWorkflows: async () => [workflow({ projectId: "project-id" })],
      listProjects: async () => [
        {
          id: "project-id",
          name: "Billing",
          enabled: true,
          repositoryMode: "managed_clone",
          repositoryUrl: "",
          localRepositoryPath: "",
          defaultBranch: "main",
          requiredRunnerLabels: [],
        },
      ],
    });
    const container = await mount(page(api));

    expect(row(container, WAITING_ID).textContent).toContain("Billing");
    expect(row(container, WAITING_ID).textContent).not.toContain("project-id");
    expect(row(container, WAITING_ID).querySelector("a")?.getAttribute("href")).toBe(`/workflows/${WAITING_ID}`);
  });
});

describe("WorkflowsPage operator controls", () => {
  it("retries terminal runs and requires a reason before blocking an active run", async () => {
    const calls: string[] = [];
    const api = {
      listProjects: async () => [],
      listWorkflows: async () => [workflow({ status: "blocked" }), workflow({ id: RUNNING_ID, status: "implementing" })],
      retryWorkflow: async (id: string) => {
        calls.push(`retry:${id}`);
        return workflow({ id, status: "recovering" });
      },
      blockWorkflow: async (id: string, reason: string) => {
        calls.push(`block:${id}:${reason}`);
        return workflow({ id, status: "blocked" });
      },
      cancelWorkflow: async () => workflow(),
    } as unknown as ApiClient;
    const container = await mount(page(api));

    await click(button(row(container, WAITING_ID), /Retry/));
    expect(calls).toEqual([`retry:${WAITING_ID}`]);
    await click(button(row(container, RUNNING_ID), /Block/));
    expect(container.querySelector(".error")?.textContent).toBe("A blocking reason is required.");
  });
});

describe("WorkflowsPage approval controls", () => {
  it("offers approve and request-changes only for a workflow waiting on a human", async () => {
    const { api } = stubApi({
      listWorkflows: async () => [
        workflow({ id: WAITING_ID, status: "waiting_human" }),
        workflow({ id: RUNNING_ID, status: "running" }),
        workflow({ id: "cccccccc-dddd-eeee-ffff-000000000000", status: "completed" }),
      ],
    });
    const container = await mount(page(api));

    expect(buttons(row(container, WAITING_ID), /Approve/)).toHaveLength(1);
    expect(buttons(row(container, WAITING_ID), /Request changes/)).toHaveLength(1);
    expect(buttons(row(container, RUNNING_ID), /./)).toHaveLength(2);
    expect(buttons(row(container, "cccccccc-dddd-eeee-ffff-000000000000"), /./)).toHaveLength(0);
    expect(container.querySelectorAll("button")).toHaveLength(6);
  });

  it("approves through the API and re-renders from the workflow the server returned", async () => {
    const { api, decisions } = stubApi({ listWorkflows: async () => [workflow()] });
    const container = await mount(page(api));

    await click(button(row(container, WAITING_ID), /Approve/));

    expect(decisions).toEqual([{ id: WAITING_ID, decision: "approved" }]);
    expect(row(container, WAITING_ID).textContent).toContain("running");
    expect(row(container, WAITING_ID).textContent).toContain("implement");
    // The decision is spent: the controls must not invite a second one.
    expect(buttons(row(container, WAITING_ID), /Approve/)).toHaveLength(0);
  });

  it("requests changes through the API with the other decision value", async () => {
    const { api, decisions } = stubApi({ listWorkflows: async () => [workflow()] });
    const container = await mount(page(api));

    await click(button(row(container, WAITING_ID), /Request changes/));

    expect(decisions).toEqual([{ id: WAITING_ID, decision: "changes_requested" }]);
  });

  it("decides the row that was clicked, not the first waiting one", async () => {
    const second = "99999999-8888-7777-6666-555555555555";
    const { api, decisions } = stubApi({
      listWorkflows: async () => [workflow(), workflow({ id: second, projectId: "ledger" })],
    });
    const container = await mount(page(api));

    await click(button(row(container, second), /Approve/));

    expect(decisions).toEqual([{ id: second, decision: "approved" }]);
    expect(row(container, WAITING_ID).textContent).toContain("waiting_human");
  });

  it("locks only the acting row while its decision is in flight", async () => {
    const second = "99999999-8888-7777-6666-555555555555";
    const pending = deferred<Workflow>();
    const { api } = stubApi({
      listWorkflows: async () => [workflow(), workflow({ id: second })],
      submitWorkflowDecision: () => pending.promise,
    });
    const container = await mount(page(api));

    await click(button(row(container, WAITING_ID), /Approve/));

    expect(button(row(container, WAITING_ID), /Approve/).disabled).toBe(true);
    expect(button(row(container, WAITING_ID), /Request changes/).disabled).toBe(true);
    expect(button(row(container, second), /Approve/).disabled).toBe(false);

    await pending.resolve(workflow({ id: WAITING_ID, status: "running" }));
    expect(button(row(container, second), /Approve/).disabled).toBe(false);
  });

  it("reports a failed decision and leaves the workflow decidable", async () => {
    const { api } = stubApi({
      listWorkflows: async () => [workflow()],
      submitWorkflowDecision: async () => {
        throw new ApiError(503, "orchestrator unavailable");
      },
    });
    const container = await mount(page(api));

    await click(button(row(container, WAITING_ID), /Approve/));

    expect(container.querySelector(".error")?.textContent).toBe(
      "Could not submit the decision. Please try again."
    );
    expect(row(container, WAITING_ID).textContent).toContain("waiting_human");
    expect(button(row(container, WAITING_ID), /Approve/).disabled).toBe(false);
  });

  it("clears a previous failure once a retry succeeds", async () => {
    let attempt = 0;
    const decisions: Decision[] = [];
    const api = {
      listProjects: async () => [],
      listWorkflows: async () => [workflow()],
      submitWorkflowDecision: async (id: string, decision: "approved" | "changes_requested") => {
        attempt += 1;
        if (attempt === 1) throw new ApiError(503, "orchestrator unavailable");
        decisions.push({ id, decision });
        return workflow({ id, status: "running" });
      },
    } as unknown as ApiClient;
    const container = await mount(page(api));

    await click(button(row(container, WAITING_ID), /Approve/));
    expect(container.querySelector(".error")).not.toBeNull();

    await click(button(row(container, WAITING_ID), /Approve/));

    expect(container.querySelector(".error")).toBeNull();
    expect(decisions).toEqual([{ id: WAITING_ID, decision: "approved" }]);
  });
});
