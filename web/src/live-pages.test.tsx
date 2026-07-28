import { act, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ApiClient } from "./api";
import { ProjectsPage } from "./projects";
import { TokensPage } from "./tokens";

class EventSourceMock {
  static instances: EventSourceMock[] = [];
  private listeners = new Map<string, (event: MessageEvent<string>) => void>();
  constructor() { EventSourceMock.instances.push(this); }
  addEventListener(type: string, listener: (event: MessageEvent<string>) => void): void { this.listeners.set(type, listener); }
  removeEventListener(type: string): void { this.listeners.delete(type); }
  close(): void {}
  emit(data: object): void { this.listeners.get("control-plane")?.({ data: JSON.stringify(data) } as MessageEvent<string>); }
}

const projectApi = (listProjects: ApiClient["listProjects"]): ApiClient => ({ listProjects } as ApiClient);
const tokenApi = (listTokens: ApiClient["listTokens"]): ApiClient => ({ listTokens } as ApiClient);

describe("live dashboard pages", () => {
  afterEach(() => { EventSourceMock.instances = []; vi.unstubAllGlobals(); });

  it("refreshes projects when an SSE event arrives", async () => {
    vi.stubGlobal("EventSource", EventSourceMock);
    const listProjects = vi.fn().mockResolvedValue([{ id: "project-1", name: "First", enabled: true }]);
    render(<ProjectsPage api={projectApi(listProjects)} />);
    await screen.findByText("First");
    listProjects.mockResolvedValue([{ id: "project-2", name: "Updated", enabled: false }]);
    await act(async () => { EventSourceMock.instances[0].emit({ id: "1", kind: "project_update", resourceId: "project-2", payload: "{}", createdAt: "now" }); });
    await waitFor(() => expect(screen.getByText("Updated")).toBeTruthy());
    expect(listProjects).toHaveBeenCalledTimes(2);
  });

  it("shows a project loading error", async () => {
    vi.stubGlobal("EventSource", EventSourceMock);
    render(<ProjectsPage api={projectApi(vi.fn().mockRejectedValue(new Error("projects unavailable")))} />);
    expect((await screen.findByRole("alert")).textContent).toContain("projects unavailable");
  });

  it("refreshes tokens when an SSE event arrives", async () => {
    vi.stubGlobal("EventSource", EventSourceMock);
    const listTokens = vi.fn().mockResolvedValue([]);
    render(<TokensPage api={tokenApi(listTokens)} />);
    await screen.findByText("No tokens");
    listTokens.mockResolvedValue([{ id: "token-1", allowedLabels: ["linux"], expiresAt: "2026-01-01T00:00:00Z" }]);
    await act(async () => { EventSourceMock.instances[0].emit({ id: "2", kind: "runner_token_created", resourceId: "token-1", payload: "{}", createdAt: "now" }); });
    await screen.findByText("linux");
    expect(listTokens).toHaveBeenCalledTimes(2);
  });
});
