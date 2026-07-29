import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { Runner } from "./api";
import { RunnersView } from "./runners";
import type { RunnersViewProps } from "./runners";

const NOW = Date.parse("2026-07-29T12:00:00Z");

function runner(overrides: Partial<Runner> = {}): Runner {
  return {
    id: "aabbccdd-1111-2222-3333-444444444444",
    name: "runner-a",
    enabled: true,
    draining: false,
    status: "online",
    labels: ["linux", "docker"],
    lastSeenAt: new Date(NOW - 3_000).toISOString(),
    ...overrides,
  };
}

function render(props: Partial<RunnersViewProps> = {}): string {
  return renderToStaticMarkup(
    <RunnersView
      runners={null}
      error={null}
      loading={false}
      now={NOW}
      onRefresh={() => undefined}
      {...props}
    />
  );
}

describe("RunnersView", () => {
  it("shows a loading line before the first response", () => {
    const html = render({ runners: null, loading: true });
    expect(html).toContain("Loading runners");
    expect(html).not.toContain("<table");
  });

  it("renders one row per runner with name, status, labels, draining and heartbeat age", () => {
    const html = render({
      runners: [
        runner({
          id: "aaaaaaaa-0000-0000-0000-000000000001",
          name: "runner-a",
          lastSeenAt: new Date(NOW - 8_000).toISOString(),
        }),
        runner({ id: "bbbbbbbb-0000-0000-0000-000000000002", name: "runner-b", draining: true }),
      ],
    });
    expect(html.match(/<tr/g)).toHaveLength(3); // header + two runners
    expect(html).toContain("runner-a");
    expect(html).toContain("runner-b");
    expect(html).toContain("aaaaaaaa");
    expect(html).toContain("Online");
    expect(html).toContain("linux");
    expect(html).toContain("docker");
    expect(html).toContain("Draining");
    expect(html).toContain("8s ago");
    expect(html).toContain("2/2 online");
  });

  it("renders every column header the acceptance criteria name", () => {
    const html = render({ runners: [runner()] });
    for (const header of ["Runner", "Status", "Labels", "Draining", "Last heartbeat"]) {
      expect(html).toContain(`<th>${header}</th>`);
    }
  });

  it("marks a runner past its heartbeat budget as stale and leaves a fresh one alone", () => {
    const html = render({
      runners: [
        runner({ id: "aaaaaaaa-0000-0000-0000-000000000001", name: "fresh" }),
        runner({
          id: "bbbbbbbb-0000-0000-0000-000000000002",
          name: "gone",
          lastSeenAt: new Date(NOW - 4 * 60_000).toISOString(),
        }),
      ],
    });
    const rows = html.split("<tr").slice(1);
    const fresh = rows.find((row) => row.includes("fresh"));
    const gone = rows.find((row) => row.includes("gone"));
    expect(fresh).not.toContain("runner-row--stale");
    expect(fresh).not.toContain(">Stale<");
    expect(gone).toContain("runner-row--stale");
    expect(gone).toContain(">Stale<");
    expect(gone).toContain("4m ago");
  });

  it("marks a runner that has never reported as stale", () => {
    const html = render({ runners: [runner({ lastSeenAt: "" })] });
    expect(html).toContain("runner-row--stale");
    expect(html).toContain("never");
  });

  it("shows an explicit empty state instead of an empty table", () => {
    const html = render({ runners: [] });
    expect(html).toContain("No runner is registered.");
    expect(html).not.toContain("<table");
  });

  it("surfaces a failed first load instead of an empty table", () => {
    const html = render({ runners: null, error: "orchestrator unavailable: connection refused" });
    expect(html).toContain('role="alert"');
    expect(html).toContain("could not be loaded");
    expect(html).toContain("orchestrator unavailable: connection refused");
    expect(html).toContain("Retry");
    expect(html).not.toContain("<table");
    expect(html).not.toContain("No runner is registered.");
    expect(html).not.toContain("Loading runners");
  });

  it("keeps the rows but warns when a refresh fails", () => {
    const html = render({ runners: [runner()], error: "orchestrator unavailable" });
    expect(html).toContain('role="alert"');
    expect(html).toContain("may be out of date");
    expect(html).toContain("<table");
    expect(html).toContain("runner-a");
  });

  it("keeps the empty state visible alongside the warning when a later refresh fails", () => {
    const html = render({ runners: [], error: "orchestrator unavailable" });
    expect(html).toContain('role="alert"');
    expect(html).toContain("No runner is registered.");
  });

  it("tolerates a runner with no labels", () => {
    const html = render({ runners: [runner({ labels: [] })] });
    expect(html).toContain("none");
  });
});
