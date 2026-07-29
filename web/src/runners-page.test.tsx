// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";
import { afterEach, describe, expect, it } from "vitest";
import { ApiError } from "./api";
import type { ApiClient, Runner } from "./api";
import { RunnersPage } from "./runners";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const mounted: Array<{ root: Root; container: HTMLElement }> = [];

afterEach(async () => {
  // Unmounting also clears the view's polling interval.
  for (const { root, container } of mounted.splice(0)) {
    await act(async () => root.unmount());
    container.remove();
  }
});

function fleet(overrides: Partial<Runner> = {}): Runner {
  return {
    id: "aabbccdd-1111-2222-3333-444444444444",
    name: "runner-a",
    enabled: true,
    draining: false,
    status: "online",
    labels: ["linux"],
    lastSeenAt: new Date().toISOString(),
    ...overrides,
  };
}

/** Minimal ApiClient stub: the page only ever calls listRunners. */
function stubApi(listRunners: ApiClient["listRunners"]): ApiClient {
  return { listRunners } as unknown as ApiClient;
}

async function mount(api: ApiClient): Promise<HTMLElement> {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  mounted.push({ root, container });
  await act(async () => {
    root.render(<RunnersPage api={api} />);
  });
  return container;
}

function clickButton(container: HTMLElement, label: string): Promise<void> {
  const button = Array.from(container.querySelectorAll("button")).find(
    (candidate) => candidate.textContent === label
  );
  if (!button) throw new Error(`no "${label}" button on screen`);
  return act(async () => {
    button.click();
  });
}

describe("RunnersPage", () => {
  it("renders the fleet it loaded", async () => {
    const container = await mount(stubApi(async () => [fleet()]));
    expect(container.querySelector("table")).not.toBeNull();
    expect(container.textContent).toContain("runner-a");
    expect(container.textContent).toContain("Online");
  });

  it("shows the failure instead of an empty table when the load fails", async () => {
    const container = await mount(
      stubApi(async () => {
        throw new ApiError(503, "orchestrator unavailable", "connection refused");
      })
    );
    expect(container.querySelector('[role="alert"]')).not.toBeNull();
    expect(container.textContent).toContain("could not be loaded");
    expect(container.textContent).toContain("orchestrator unavailable");
    expect(container.querySelector("table")).toBeNull();
    expect(container.textContent).not.toContain("No runner is registered.");
    expect(container.textContent).not.toContain("Loading runners");
  });

  it("shows the empty state, not a failure, when the fleet is genuinely empty", async () => {
    const container = await mount(stubApi(async () => []));
    expect(container.querySelector('[role="alert"]')).toBeNull();
    expect(container.textContent).toContain("No runner is registered.");
  });

  it("recovers when a retry succeeds", async () => {
    let attempt = 0;
    const container = await mount(
      stubApi(async () => {
        attempt += 1;
        if (attempt === 1) throw new ApiError(503, "orchestrator unavailable");
        return [fleet()];
      })
    );
    expect(container.querySelector("table")).toBeNull();
    await clickButton(container, "Retry");
    expect(container.querySelector('[role="alert"]')).toBeNull();
    expect(container.textContent).toContain("runner-a");
  });

  it("keeps the last known fleet on screen when a refresh fails, and says so", async () => {
    let attempt = 0;
    const container = await mount(
      stubApi(async () => {
        attempt += 1;
        if (attempt === 1) return [fleet()];
        throw new ApiError(503, "orchestrator unavailable");
      })
    );
    expect(container.textContent).toContain("runner-a");
    await clickButton(container, "Refresh");
    expect(container.querySelector('[role="alert"]')).not.toBeNull();
    expect(container.textContent).toContain("may be out of date");
    expect(container.textContent).toContain("runner-a");
  });

  it("marks a runner that has stopped heartbeating as stale", async () => {
    const container = await mount(
      stubApi(async () => [
        fleet({ id: "aaaa1111-0000-0000-0000-000000000001", name: "fresh" }),
        fleet({
          id: "bbbb2222-0000-0000-0000-000000000002",
          name: "gone",
          lastSeenAt: new Date(Date.now() - 10 * 60_000).toISOString(),
        }),
      ])
    );
    const rows = Array.from(container.querySelectorAll("tbody tr"));
    expect(rows).toHaveLength(2);
    const fresh = rows.find((row) => row.textContent?.includes("fresh"));
    const gone = rows.find((row) => row.textContent?.includes("gone"));
    expect(fresh?.className).not.toContain("runner-row--stale");
    expect(fresh?.textContent).not.toContain("Stale");
    expect(gone?.className).toContain("runner-row--stale");
    expect(gone?.textContent).toContain("Stale");
    expect(gone?.textContent).toContain("10m ago");
  });

  it("does not report an aborted in-flight load as a failure after unmount", async () => {
    const container = document.createElement("div");
    document.body.appendChild(container);
    const root = createRoot(container);
    let reject: (error: unknown) => void = () => undefined;
    const pending = new Promise<Runner[]>((_, rejectPending) => {
      reject = rejectPending;
    });
    await act(async () => {
      root.render(<RunnersPage api={stubApi(() => pending)} />);
    });
    await act(async () => root.unmount());
    await act(async () => {
      reject(new DOMException("The operation was aborted.", "AbortError"));
      await pending.catch(() => undefined);
    });
    expect(container.textContent).toBe("");
    container.remove();
  });
});
