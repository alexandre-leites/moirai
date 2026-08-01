// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { QueuePage } from "./queue";
import { button, unmountAll } from "./test-dom";
import { NOW, VIEWER, mountView, queueEntry, stubApi } from "./test-console";

afterEach(unmountAll);

describe("QueuePage", () => {
  it("numbers the queue in scheduler order and says why each issue is held", async () => {
    const api = stubApi({
      listQueue: async () => [
        queueEntry({ externalId: "#62", priority: 8, blockedReason: "project_circuit_open" }),
        queueEntry({ externalId: "#104", priority: 5, blockedReason: "project_locked" }),
        queueEntry({ externalId: "#105", priority: 1, blockedReason: "" }),
      ],
    });
    const container = await mountView(<QueuePage api={api} />, api);

    // The first table is the queue; the second is the issue-sync card below it.
    const rows = container.querySelectorAll("table")[0].querySelectorAll("tbody tr");
    expect(rows).toHaveLength(3);
    expect(rows[0].textContent).toContain("P8");
    expect(rows[0].textContent).toContain("Project circuit open");
    expect(rows[1].textContent).toContain("Project busy");
    expect(rows[2].textContent).toContain("Next to schedule");
  });

  it("shows the empty state when nothing is waiting", async () => {
    const api = stubApi();
    const container = await mountView(<QueuePage api={api} />, api);
    expect(container.textContent).toContain("The queue is empty");
  });

  it("surfaces a load failure instead of an empty queue", async () => {
    const api = stubApi({ listQueue: async () => { throw new Error("orchestrator unreachable"); } });
    const container = await mountView(<QueuePage api={api} />, api);
    expect(container.textContent).toContain("orchestrator unreachable");
    expect(container.textContent).not.toContain("The queue is empty");
  });

  it("reports issue-sync backoff with its error and next retry", async () => {
    const api = stubApi({
      issueSyncStatus: async () => [
        {
          projectId: "p1", projectName: "atlas-web", enabled: true, issueCount: 12, eligibleCount: 3,
          lastSyncedAt: NOW, consecutiveFailures: 3, nextRetryAt: NOW,
          lastError: "gh: HTTP 502 from api.github.com", backingOff: true,
        },
        {
          projectId: "p2", projectName: "chronos", enabled: false, issueCount: 0, eligibleCount: 0,
          consecutiveFailures: 0, backingOff: false,
        },
      ],
    });
    const container = await mountView(<QueuePage api={api} />, api);

    expect(container.textContent).toContain("Backing off");
    expect(container.textContent).toContain("gh: HTTP 502 from api.github.com");
    expect(container.textContent).toContain("Project paused");
  });

  it("offers Sync now to an admin and hides it from a viewer", async () => {
    const admin = stubApi();
    const adminView = await mountView(<QueuePage api={admin} />, admin);
    expect(button(adminView, /Sync now/)).toBeTruthy();

    const viewer = stubApi({ me: async () => VIEWER });
    const viewerView = await mountView(<QueuePage api={viewer} />, viewer);
    expect(viewerView.textContent).not.toContain("Sync now");
  });
});
