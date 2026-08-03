// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { PhaseThread } from "./ui/thread";
import { mount, unmountAll } from "./test-dom";
import { workflow } from "./test-console";

afterEach(unmountAll);

const paths = (container: HTMLElement) => Array.from(container.querySelectorAll("path"));
/** The dashed path is the "still ahead" one; the plain stroke is the spun thread. */
const ahead = (container: HTMLElement) => paths(container).find((path) => path.getAttribute("stroke-dasharray"));
const spun = (container: HTMLElement) => paths(container).find((path) => !path.getAttribute("stroke-dasharray"));
const fray = (container: HTMLElement) => paths(container).find((path) => path.getAttribute("stroke") === "var(--crit)");

describe("PhaseThread", () => {
  it("labels itself with the run's status for screen readers", async () => {
    const container = await mount(<PhaseThread workflow={workflow({ status: "waiting_human" })} />);
    expect(container.querySelector("svg")?.getAttribute("aria-label"))
      .toBe("Workflow phase progress: Needs decision");
  });

  it("spins thread behind the run and leaves the rest dashed", async () => {
    const container = await mount(<PhaseThread workflow={workflow({ status: "preparing", reviewCycles: 1 })} />);
    // Cubic curves, not straight lines: the spun thread has to read as thread.
    expect(spun(container)?.getAttribute("d")).toContain("C");
    expect(ahead(container)?.getAttribute("d")).toContain("L");
    expect(fray(container)).toBeUndefined();
  });

  it("draws the spindle on the current phase", async () => {
    const container = await mount(<PhaseThread workflow={workflow({ status: "preparing" })} />);
    expect(container.querySelector(".spindle-core")).not.toBeNull();
  });

  it("cuts the thread where a blocked run stopped, and draws no spindle", async () => {
    const container = await mount(
      <PhaseThread workflow={workflow({ status: "blocked", pipelineRepairAttempts: 3 })} />
    );
    expect(fray(container)).not.toBeUndefined();
    expect(container.querySelector(".spindle-core")).toBeNull();
  });

  it("cuts a failed run at the checks it never cleared", async () => {
    const container = await mount(
      <PhaseThread workflow={workflow({ status: "failed", pullRequestUrl: "https://example.test/pull/9", pullRequestState: "open" })} />
    );
    const cut = fray(container)!.getAttribute("d")!;
    const checksX = cut.split(" ")[1];
    // The fray starts at the checks node, well past the middle of the path.
    expect(Number(checksX)).toBeGreaterThan(600);
  });

  it("runs the thread to the end for a delivered run", async () => {
    const container = await mount(<PhaseThread workflow={workflow({ status: "completed" })} />);
    expect(ahead(container)?.getAttribute("d")).toBe("");
    expect(fray(container)).toBeUndefined();
  });

  it("drops labels and rings in the mini variant", async () => {
    const container = await mount(<PhaseThread workflow={workflow()} mini />);
    expect(container.querySelectorAll("text")).toHaveLength(0);
    expect(container.querySelector("svg")?.getAttribute("class")).toBe("minithread");
  });
});
