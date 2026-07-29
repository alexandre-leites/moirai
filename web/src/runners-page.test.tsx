// @vitest-environment jsdom
import { act } from "react";
import { createRoot } from "react-dom/client";
import type { Root } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import type { ApiClient, Runner } from "./api";
import { RunnersPage } from "./runners";
import { POLL_INTERVAL_MS } from "./runner-status";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const mounted: Array<{ root: Root; container: HTMLElement }> = [];

async function unmountAll(): Promise<void> {
  for (const { root, container } of mounted.splice(0)) {
    await act(async () => root.unmount());
    container.remove();
  }
}

afterEach(unmountAll);

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

function click(container: HTMLElement, label: RegExp): Promise<void> {
  const button = Array.from(container.querySelectorAll("button")).find((candidate) =>
    label.test(candidate.textContent ?? "")
  );
  if (!button) throw new Error(`no button matching ${label} on screen`);
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

  it("shows the failure when the fetch never reaches the API at all", async () => {
    const container = await mount(
      stubApi(async () => {
        throw new TypeError("Failed to fetch");
      })
    );
    expect(container.querySelector('[role="alert"]')).not.toBeNull();
    expect(container.textContent).toContain("Failed to fetch");
    expect(container.querySelector("table")).toBeNull();
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
    await click(container, /^Retry$/);
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
    await click(container, /^Refresh/);
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
          lastSeenAt: new Date(Date.now() - 10 * 60_000 - 5_000).toISOString(),
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
    expect(container.textContent).toContain("1/2 online");
  });

  it("does not let an abandoned request overwrite a newer one", async () => {
    const pending: Array<(runners: Runner[]) => void> = [];
    const container = await mount(
      stubApi(
        () =>
          new Promise<Runner[]>((resolve) => {
            pending.push(resolve);
          })
      )
    );
    expect(container.textContent).toContain("Loading runners");

    // Refresh abandons request #1 and starts #2.
    await click(container, /^Refresh/);
    expect(pending).toHaveLength(2);

    await act(async () => pending[1]([fleet({ name: "current" })]));
    expect(container.textContent).toContain("current");

    // The abandoned request now answers, with the older snapshot.
    await act(async () => pending[0]([fleet({ name: "outdated" })]));
    expect(container.textContent).toContain("current");
    expect(container.textContent).not.toContain("outdated");
  });

  it("measures heartbeat age from when the orchestrator was asked, not when it answered", async () => {
    vi.useFakeTimers();
    try {
      let resolveRequest: ((runners: Runner[]) => void) | null = null;
      // The runner beat 8s before the page asked; the answer takes 35s to
      // arrive. Measuring from arrival would report it 43s old — past the
      // 30s budget — and paint a healthy runner stale.
      const lastSeenAt = new Date(Date.now() - 8_000).toISOString();
      const container = await mount(
        stubApi(
          () =>
            new Promise<Runner[]>((resolve) => {
              resolveRequest = resolve;
            })
        )
      );
      await act(async () => {
        vi.advanceTimersByTime(35_000);
      });
      await act(async () => resolveRequest?.([fleet({ lastSeenAt })]));

      expect(container.querySelector("tbody tr")?.className).not.toContain("runner-row--stale");
      expect(container.textContent).toContain("8s ago");
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("RunnersPage polling", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("refreshes on the poll interval while the tab is visible", async () => {
    vi.useFakeTimers();
    let calls = 0;
    await mount(
      stubApi(async () => {
        calls += 1;
        return [fleet()];
      })
    );
    expect(calls).toBe(1);
    await act(async () => {
      vi.advanceTimersByTime(POLL_INTERVAL_MS);
    });
    expect(calls).toBe(2);
  });

  it("pauses polling while the tab is hidden", async () => {
    vi.useFakeTimers();
    let calls = 0;
    await mount(
      stubApi(async () => {
        calls += 1;
        return [fleet()];
      })
    );
    const hidden = vi.spyOn(document, "hidden", "get").mockReturnValue(true);
    try {
      await act(async () => {
        vi.advanceTimersByTime(POLL_INTERVAL_MS * 3);
      });
      expect(calls).toBe(1);
    } finally {
      hidden.mockRestore();
    }
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(calls).toBe(2);
  });

  it("never stacks requests when the orchestrator is slow to answer", async () => {
    vi.useFakeTimers();
    let calls = 0;
    await mount(
      stubApi(() => {
        calls += 1;
        return new Promise<Runner[]>(() => undefined);
      })
    );
    await act(async () => {
      vi.advanceTimersByTime(POLL_INTERVAL_MS * 5);
    });
    expect(calls).toBe(1);
  });

  it("stops polling once the page is unmounted", async () => {
    vi.useFakeTimers();
    let calls = 0;
    await mount(
      stubApi(async () => {
        calls += 1;
        return [fleet()];
      })
    );
    await unmountAll();
    await act(async () => {
      vi.advanceTimersByTime(POLL_INTERVAL_MS * 5);
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(calls).toBe(1);
  });
});
