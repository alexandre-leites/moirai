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
      isAdmin={true}
      {...props}
    />
  );
}

/** The markup of one `<tbody>` row, found by the runner name it contains. */
function row(html: string, name: string): string {
  const found = html
    .split("<tr")
    .slice(1)
    .find((candidate) => candidate.includes(`<div>${name}</div>`));
  if (!found) throw new Error(`no row for "${name}" in:\n${html}`);
  return found;
}

/** The contents of one `<td>` of a row, counting from zero. */
function cell(rowHtml: string, index: number): string {
  const cells = rowHtml.split("<td>").slice(1);
  if (cells.length <= index) throw new Error(`row has ${cells.length} cells, wanted #${index}`);
  return cells[index].split("</td>")[0];
}

const DRAINING_CELL = 3;
const HEARTBEAT_CELL = 4;

describe("RunnersView", () => {
  it("shows a loading line before the first response", () => {
    const html = render({ runners: null, loading: true });
    expect(html).toContain("Loading runners");
    expect(html).not.toContain("<table");
  });

  it("renders one row per runner with name, status, labels and heartbeat age", () => {
    const html = render({
      runners: [
        runner({
          id: "aaaaaaaa-0000-0000-0000-000000000001",
          name: "runner-a",
          lastSeenAt: new Date(NOW - 8_000).toISOString(),
        }),
        runner({ id: "bbbbbbbb-0000-0000-0000-000000000002", name: "runner-b" }),
      ],
    });
    expect(html.match(/<tr/g)).toHaveLength(3); // header + two runners
    expect(html).toContain("aaaaaaaa");
    expect(row(html, "runner-a")).toContain("Online");
    expect(row(html, "runner-a")).toContain("linux");
    expect(row(html, "runner-a")).toContain("docker");
    expect(cell(row(html, "runner-a"), HEARTBEAT_CELL)).toContain("8s ago");
    expect(cell(row(html, "runner-b"), HEARTBEAT_CELL)).toContain("just now");
  });

  it("shows the draining flag in its own cell, per runner", () => {
    const html = render({
      runners: [
        runner({ id: "aaaaaaaa-0000-0000-0000-000000000001", name: "steady" }),
        runner({ id: "bbbbbbbb-0000-0000-0000-000000000002", name: "leaving", draining: true }),
      ],
    });
    expect(cell(row(html, "leaving"), DRAINING_CELL)).toContain("Draining");
    expect(cell(row(html, "steady"), DRAINING_CELL)).not.toContain("Draining");
    expect(cell(row(html, "steady"), DRAINING_CELL)).toContain("No");
  });

  it("renders drain, undrain, and revoke controls when actions are available", () => {
    const drain = render({ runners: [runner()], onSetState: () => undefined });
    expect(row(drain, "runner-a")).toContain(">Drain<");
    expect(row(drain, "runner-a")).toContain(">Revoke<");
    const undrain = render({ runners: [runner({ draining: true })], onSetState: () => undefined });
    expect(row(undrain, "runner-a")).toContain(">Undrain<");
  });

  it("carries the absolute heartbeat time in the cell's title attribute", () => {
    const seenAt = new Date(NOW - 8_000);
    const html = render({ runners: [runner({ lastSeenAt: seenAt.toISOString() })] });
    expect(cell(row(html, "runner-a"), HEARTBEAT_CELL)).toContain(`title="${seenAt.toLocaleString()}"`);
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
    expect(row(html, "fresh")).not.toContain("runner-row--stale");
    expect(row(html, "fresh")).not.toContain(">Stale<");
    expect(row(html, "fresh")).toContain(">Online<");
    // Stale is signalled three ways, none of them colour alone: the status
    // pill, a badge next to the age, and the row stripe.
    expect(row(html, "gone")).toContain("runner-row--stale");
    expect(cell(row(html, "gone"), 1)).toContain(">Stale<");
    expect(cell(row(html, "gone"), HEARTBEAT_CELL)).toContain(">Stale<");
    expect(cell(row(html, "gone"), HEARTBEAT_CELL)).toContain("4m ago");
  });

  it("marks a runner that has never reported as stale", () => {
    const html = render({ runners: [runner({ lastSeenAt: "" })] });
    expect(html).toContain("runner-row--stale");
    expect(html).toContain("never");
  });

  it("counts only reachable runners in the header, not the row count", () => {
    const html = render({
      runners: [
        runner({ id: "aaaaaaaa-0000-0000-0000-000000000001", name: "up" }),
        runner({ id: "bbbbbbbb-0000-0000-0000-000000000002", name: "down", status: "offline" }),
        runner({
          id: "cccccccc-0000-0000-0000-000000000003",
          name: "silent",
          lastSeenAt: new Date(NOW - 4 * 60_000).toISOString(),
        }),
      ],
    });
    expect(html).toContain("1/3 online");
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

  it("leaves the refresh control usable while a request is outstanding", () => {
    // A hung request must not lock the operator out of retrying.
    expect(render({ runners: [runner()], loading: true })).not.toContain("disabled");
  });

  it("tolerates a runner with no labels", () => {
    const html = render({ runners: [runner({ labels: [] })] });
    expect(cell(row(html, "runner-a"), 2)).toContain("none");
  });
});
