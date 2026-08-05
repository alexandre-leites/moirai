// @vitest-environment jsdom
import { act } from "react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it } from "vitest";
import type { ApiClient } from "./api";
import { AuthProvider } from "./auth";
import { Console } from "./shell";
import { routeTitle } from "./route-title";
import { ToastProvider } from "./ui/toast";
import { click, mount, unmountAll } from "./test-dom";
import { VIEWER, project, queueEntry, runner, stubApi, workflow } from "./test-console";

afterEach(unmountAll);

async function mountConsole(api: ApiClient, route = "/"): Promise<HTMLElement> {
  return mount(
    <MemoryRouter initialEntries={[route]}>
      <AuthProvider api={api}>
        <ToastProvider>
          <Console api={api} />
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>
  );
}

const populated = () => stubApi({
  listProjects: async () => [project()],
  listWorkflows: async () => [
    workflow({ id: "wf-1", status: "preparing" }),
    workflow({ id: "wf-2", status: "waiting_human" }),
    workflow({ id: "wf-3", status: "completed" }),
  ],
  listRunners: async () => [runner(), runner({ id: "r2", name: "loom-03", status: "offline" })],
  listQueue: async () => [queueEntry(), queueEntry({ externalId: "#105" })],
});

const navCount = (container: HTMLElement, label: string): string | undefined =>
  Array.from(container.querySelectorAll(".side .nav-item"))
    .find((item) => item.textContent?.startsWith(label))
    ?.querySelector(".count")?.textContent ?? undefined;

describe("routeTitle", () => {
  it("names the view in the document title", () => {
    expect(routeTitle("/")).toBe("Overview — Moirai Console");
    expect(routeTitle("/queue")).toBe("Queue — Moirai Console");
    expect(routeTitle("/workflows")).toBe("Workflows — Moirai Console");
    expect(routeTitle("/workflows/wf-1")).toBe("Workflow — Moirai Console");
    expect(routeTitle("/nowhere")).toBe("Moirai Console");
  });
});

describe("Console shell", () => {
  it("shows live counts in the sidebar", async () => {
    const container = await mountConsole(populated());
    expect(navCount(container, "Queue")).toBe("2");
    expect(navCount(container, "Workflows")).toBe("2");
    expect(navCount(container, "Runners")).toBe("1/2");
  });

  it("marks the workflow count hot when something needs a human", async () => {
    const hot = await mountConsole(populated());
    expect(hot.querySelector(".side .count.hot")).not.toBeNull();

    const calm = await mountConsole(stubApi({ listWorkflows: async () => [workflow({ status: "preparing" })] }));
    expect(calm.querySelector(".side .count.hot")).toBeNull();
  });

  it("sets the document title from the route", async () => {
    await mountConsole(populated(), "/queue");
    expect(document.title).toBe("Queue — Moirai Console");
  });

  it("renders a 404 with a way back for an unknown route", async () => {
    const container = await mountConsole(populated(), "/nowhere");
    expect(container.textContent).toContain("That address does not match a console view");
    expect(container.querySelector("a[href='/']")).not.toBeNull();
  });

  it("redirects the retired token page to the runner fleet", async () => {
    const container = await mountConsole(populated(), "/tokens");
    expect(container.textContent).toContain("Registration tokens");
    expect(container.querySelector("h1")?.textContent).toBe("Runners");
  });

  it("opens a focus-trapped drawer that Escape closes", async () => {
    const container = await mountConsole(populated());

    await click(container.querySelector<HTMLButtonElement>("button[aria-label='Open navigation']")!);
    const drawer = container.querySelector(".drawer")!;
    expect(drawer).not.toBeNull();
    // Focus moved into the drawer rather than staying on the trigger.
    expect(drawer.contains(document.activeElement)).toBe(true);

    await act(async () => {
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    });
    expect(container.querySelector(".drawer")).toBeNull();
  });

  it("sends a signed-out visitor to the login page", async () => {
    const api = stubApi({ me: async () => { throw new Error("unauthenticated"); } });
    const container = await mountConsole(api);
    expect(container.textContent).not.toContain("Operate");
  });

  it("names the signed-in user and their role", async () => {
    const container = await mountConsole(stubApi({ me: async () => VIEWER }));
    expect(container.querySelector(".side .who")?.textContent).toBe("vera · viewer");
  });

  it("reports the orchestrator as unreachable when the snapshot fails", async () => {
    const api = stubApi({ listWorkflows: async () => { throw new Error("down"); } });
    const container = await mountConsole(api);
    expect(container.querySelector(".side .foot")?.textContent).toContain("orchestrator: unreachable");
  });
});
