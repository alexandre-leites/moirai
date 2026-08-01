import { describe, expect, it } from "vitest";
import { absolute, age, ageAgo, clock, holdReason, plural } from "./format";

const NOW = Date.parse("2026-08-01T12:00:00Z");
const ago = (seconds: number) => new Date(NOW - seconds * 1000).toISOString();

describe("age", () => {
  it("shortens as the age grows, and keeps the leading unit meaningful", () => {
    expect(age(ago(12), NOW)).toBe("12s");
    expect(age(ago(59), NOW)).toBe("59s");
    expect(age(ago(60), NOW)).toBe("1m");
    expect(age(ago(3599), NOW)).toBe("59m");
    expect(age(ago(3600), NOW)).toBe("1h");
    expect(age(ago(3600 + 20 * 60), NOW)).toBe("1h 20m");
    expect(age(ago(2 * 86400), NOW)).toBe("2d");
    expect(age(ago(2 * 86400 + 3 * 3600), NOW)).toBe("2d 3h");
  });

  it("collapses a timestamp from the future to zero rather than a negative age", () => {
    // The orchestrator stamps the time and the browser reads it; the two clocks
    // disagreeing by a second is normal and must not render as "-1s ago".
    expect(age(new Date(NOW + 5000).toISOString(), NOW)).toBe("0s");
  });

  it("returns null for a missing or unreadable timestamp so callers can say why", () => {
    expect(age(undefined, NOW)).toBeNull();
    expect(age("", NOW)).toBeNull();
    expect(age("not a date", NOW)).toBeNull();
  });
});

describe("ageAgo", () => {
  it("suffixes a usable age and falls back otherwise", () => {
    expect(ageAgo(ago(90), "never", NOW)).toBe("1m ago");
    expect(ageAgo(undefined, "never", NOW)).toBe("never");
  });
});

describe("absolute", () => {
  it("passes an unparseable value through rather than rendering Invalid Date", () => {
    expect(absolute("whenever")).toBe("whenever");
    expect(absolute(undefined)).toBeUndefined();
    expect(absolute(ago(0))).not.toContain("Invalid");
  });
});

describe("clock", () => {
  it("keeps an unreadable timestamp visible instead of blanking the row", () => {
    expect(clock("nonsense")).toBe("nonsense");
    expect(clock(ago(0))).toMatch(/^\d{2}:\d{2}:\d{2}$/);
  });
});

describe("plural", () => {
  it("agrees with the count", () => {
    expect(plural(1, "item")).toBe("1 item");
    expect(plural(0, "item")).toBe("0 items");
    expect(plural(2, "entry", "entries")).toBe("2 entries");
  });
});

describe("holdReason", () => {
  it("puts the scheduler's reasons in operator English", () => {
    expect(holdReason("project_locked")).toBe("Project busy — one workflow per project");
    expect(holdReason("no_matching_runner")).toBe("No runner matches this project's required labels");
    expect(holdReason("")).toBe("Next to schedule");
  });

  it("shows a reason added server-side rather than swallowing it", () => {
    expect(holdReason("some_new_reason")).toBe("some new reason");
  });
});
