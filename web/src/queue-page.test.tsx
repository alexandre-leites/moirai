// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";
import { afterEach, describe, expect, it } from "vitest";
import type { ApiClient, QueueEntry } from "./api";
import { QueuePage } from "./queue";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const mounted: Array<{ root: Root; container: HTMLElement }> = [];

async function unmountAll(): Promise<void> {
  for (const { root, container } of mounted.splice(0)) {
    await act(async () => root.unmount());
    container.remove();
  }
}

afterEach(unmountAll);

function entry(overrides: Partial<QueueEntry> = {}): QueueEntry {
  return {
    projectId: "00000000-0000-0000-0000-000000000001",
    projectName: "Alpha",
    externalId: "42",
    title: "Implement queue",
    priority: 100,
    blockedReason: "",
    ...overrides,
  };
}

/** Minimal ApiClient stub: the page only ever calls listQueue. */
function stubApi(listQueue: ApiClient["listQueue"]): ApiClient {
  return { listQueue } as unknown as ApiClient;
}

async function mount(api: ApiClient): Promise<HTMLElement> {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  mounted.push({ root, container });
  await act(async () => {
    root.render(<QueuePage api={api} />);
  });
  return container;
}

describe("QueuePage", () => {
  it("renders the entries it loaded and maps blocked reasons", async () => {
    const container = await mount(stubApi(async () => [
      entry(),
      entry({ projectName: "Beta", externalId: "7", title: "Lower priority", priority: 10, blockedReason: "no_matching_runner" }),
    ]));
    expect(container.querySelector("table")).not.toBeNull();
    expect(container.textContent).toContain("Implement queue");
    expect(container.textContent).toContain("100");
    expect(container.textContent).toContain("No matching runner available");
  });

  it("shows a plain blocked reason when it is not one of the known keys", async () => {
    const container = await mount(stubApi(async () => [entry({ blockedReason: "project_locked" })]));
    expect(container.textContent).toContain("Project has an active workflow");
  });

  it("shows the empty state when no entries are waiting", async () => {
    const container = await mount(stubApi(async () => []));
    expect(container.querySelector("table")).toBeNull();
    expect(container.textContent).toContain("The queue is empty");
  });

  it("surfaces a load error", async () => {
    const container = await mount(stubApi(async () => {
      throw new Error("boom");
    }));
    expect(container.textContent).toContain("Could not load the queue");
  });
});
