// @vitest-environment jsdom
import { act } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ConsoleDataProvider, useConsoleData } from "./console-data";
import { mount, unmountAll } from "./test-dom";
import { stubApi, workflow } from "./test-console";

afterEach(async () => {
  await unmountAll();
  vi.unstubAllGlobals();
  FakeEventSource.instances = [];
});

/**
 * Stands in for the browser's EventSource: subscribeEvents (events.ts) only
 * ever calls addEventListener and close on it, so that's all this needs to
 * implement. `emit` lets a test fire a named SSE event as the server would.
 */
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  private listeners: Record<string, Array<(event: MessageEvent) => void>> = {};

  constructor(public url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: EventListener): void {
    (this.listeners[type] ??= []).push(listener as (event: MessageEvent) => void);
  }

  close(): void {
    // no-op: nothing in these tests asserts on disconnect.
  }

  emit(type: string, payload: unknown): void {
    const event = { data: JSON.stringify(payload) } as MessageEvent<string>;
    for (const listener of this.listeners[type] ?? []) listener(event);
  }
}

function Probe({ id }: { id: string }) {
  const { data } = useConsoleData();
  const found = data?.workflows.find((candidate) => candidate.id === id);
  if (!found) return <div data-testid="row">missing</div>;
  return (
    <div data-testid="row">
      {found.status}|{found.phase}|{found.issueTitle}|{found.pullRequestUrl}|{found.planningAttempts}
    </div>
  );
}

describe("ConsoleDataProvider workflow SSE events", () => {
  it("patches an existing row instead of replacing it with the SSE lifecycle stub", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    const full = workflow({
      id: "wf-1",
      status: "preparing",
      phase: "preparing",
      issueTitle: "Fix the thing",
      pullRequestUrl: "https://example.test/pull/9",
      planningAttempts: 3,
    });
    const api = stubApi({ listWorkflows: async () => [full] });

    const container = await mount(
      <ConsoleDataProvider api={api}>
        <Probe id="wf-1" />
      </ConsoleDataProvider>
    );

    expect(container.querySelector('[data-testid="row"]')?.textContent)
      .toBe("preparing|preparing|Fix the thing|https://example.test/pull/9|3");

    const source = FakeEventSource.instances.at(-1);
    expect(source).toBeDefined();

    // The real server only ever sends the lifecycle shape here (see
    // workflowPayload in api/internal/http/handlers/events.go) — no
    // issueTitle, pullRequestUrl, or attempt counters.
    await act(async () => {
      source!.emit("workflow", {
        type: "workflow",
        workflow: { id: "wf-1", projectId: "project-1", status: "implementing", phase: "implementing" },
      });
    });

    expect(container.querySelector('[data-testid="row"]')?.textContent)
      .toBe("implementing|implementing|Fix the thing|https://example.test/pull/9|3");
  });

  it("inserts a lifecycle-only row for a workflow the snapshot hasn't loaded yet", async () => {
    vi.stubGlobal("EventSource", FakeEventSource);
    const api = stubApi({ listWorkflows: async () => [] });

    const container = await mount(
      <ConsoleDataProvider api={api}>
        <Probe id="wf-new" />
      </ConsoleDataProvider>
    );

    expect(container.querySelector('[data-testid="row"]')?.textContent).toBe("missing");

    const source = FakeEventSource.instances.at(-1);
    expect(source).toBeDefined();
    await act(async () => {
      source!.emit("workflow", {
        type: "workflow",
        workflow: { id: "wf-new", projectId: "project-1", status: "preparing", phase: "preparing" },
      });
    });

    expect(container.querySelector('[data-testid="row"]')?.textContent).toBe("preparing|preparing|||");
  });
});
