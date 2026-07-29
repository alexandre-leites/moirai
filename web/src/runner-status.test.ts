import { describe, expect, it } from "vitest";
import { ApiError, createApiClient } from "./api";
import type { ApiClient, Runner } from "./api";
import {
  countOnline,
  describeHeartbeat,
  describeLoadError,
  describeRunnerStatus,
  formatAge,
  loadRunners,
  STALE_AFTER_MS,
} from "./runner-status";

const NOW = Date.parse("2026-07-29T12:00:00Z");

function runner(overrides: Partial<Runner> = {}): Runner {
  return {
    id: "11111111-2222-3333-4444-555555555555",
    name: "runner-a",
    enabled: true,
    draining: false,
    status: "online",
    labels: ["linux", "docker"],
    lastSeenAt: new Date(NOW - 2_000).toISOString(),
    ...overrides,
  };
}

const fresh = describeHeartbeat(new Date(NOW - 2_000).toISOString(), NOW);
const missed = describeHeartbeat(new Date(NOW - 10 * 60_000).toISOString(), NOW);

describe("formatAge", () => {
  it("reads durations the way an operator does", () => {
    expect(formatAge(0)).toBe("just now");
    expect(formatAge(4_999)).toBe("just now");
    expect(formatAge(5_000)).toBe("5s ago");
    expect(formatAge(59_000)).toBe("59s ago");
    expect(formatAge(60_000)).toBe("1m ago");
    expect(formatAge(59 * 60_000)).toBe("59m ago");
    expect(formatAge(60 * 60_000)).toBe("1h ago");
    expect(formatAge(25 * 60 * 60_000)).toBe("1d ago");
  });

  it("does not render a negative age when the two clocks disagree", () => {
    expect(formatAge(-30_000)).toBe("just now");
  });
});

describe("describeHeartbeat", () => {
  it("is fresh inside the heartbeat budget", () => {
    const heartbeat = describeHeartbeat(new Date(NOW - 8_000).toISOString(), NOW);
    expect(heartbeat.stale).toBe(false);
    expect(heartbeat.label).toBe("8s ago");
  });

  it("is not stale exactly at the budget, and stale one millisecond past it", () => {
    expect(describeHeartbeat(new Date(NOW - STALE_AFTER_MS).toISOString(), NOW).stale).toBe(false);
    expect(describeHeartbeat(new Date(NOW - STALE_AFTER_MS - 1).toISOString(), NOW).stale).toBe(true);
  });

  it("is stale after several missed heartbeats", () => {
    const heartbeat = describeHeartbeat(new Date(NOW - 5 * 60_000).toISOString(), NOW);
    expect(heartbeat.stale).toBe(true);
    expect(heartbeat.label).toBe("5m ago");
  });

  it("treats a runner that never reported as stale rather than fresh", () => {
    for (const value of ["", null, undefined]) {
      const heartbeat = describeHeartbeat(value, NOW);
      expect(heartbeat.stale).toBe(true);
      expect(heartbeat.label).toBe("never");
    }
  });

  it("separates an unreadable timestamp from a runner that never reported", () => {
    const heartbeat = describeHeartbeat("not-a-timestamp", NOW);
    expect(heartbeat.stale).toBe(true);
    expect(heartbeat.label).toBe("unknown");
    expect(heartbeat.title).toContain("not-a-timestamp");
  });

  it("reads the orchestrator's own timestamp format, offset and microseconds included", () => {
    // What `datetime.isoformat()` emits for the TIMESTAMPTZ column, per
    // orchestrator/src/moirai/grpc/control_plane.py.
    // 7.071s before NOW: the offset is honoured (a naive read would be hours
    // out) and the sub-millisecond digits are truncated, not rejected.
    const heartbeat = describeHeartbeat("2026-07-29T11:59:52.928921+00:00", NOW);
    expect(heartbeat.label).toBe("7s ago");
    expect(heartbeat.stale).toBe(false);
  });

  it("keeps the absolute timestamp for the title attribute", () => {
    const heartbeat = describeHeartbeat(new Date(NOW - 8_000).toISOString(), NOW);
    expect(heartbeat.title).toBe(new Date(NOW - 8_000).toLocaleString());
  });

  it("reads a clock-skewed future heartbeat as fresh", () => {
    expect(describeHeartbeat(new Date(NOW + 30_000).toISOString(), NOW)).toMatchObject({
      label: "just now",
      stale: false,
    });
  });
});

describe("describeRunnerStatus", () => {
  it("maps the fleet states to the specification's pills", () => {
    expect(describeRunnerStatus(runner(), fresh)).toEqual({ label: "Online", variant: "ok" });
    expect(describeRunnerStatus(runner({ draining: true }), fresh))
      .toEqual({ label: "Draining", variant: "warn" });
    expect(describeRunnerStatus(runner({ enabled: false }), fresh))
      .toEqual({ label: "Disabled", variant: "idle" });
    expect(describeRunnerStatus(runner({ status: "offline" }), fresh))
      .toEqual({ label: "Offline", variant: "bad" });
  });

  it("does not report a runner that stopped heartbeating as online", () => {
    // `runners.status` only returns to 'offline' through lease expiry or
    // revocation, so a killed idle runner keeps status = 'online' forever.
    expect(describeRunnerStatus(runner(), missed)).toEqual({ label: "Stale", variant: "warn" });
    expect(describeRunnerStatus(runner({ draining: true }), missed))
      .toEqual({ label: "Stale", variant: "warn" });
    expect(describeRunnerStatus(runner({ enabled: false }), missed))
      .toEqual({ label: "Stale", variant: "warn" });
  });

  it("reports a disconnected runner as offline even while it is draining or stale", () => {
    expect(describeRunnerStatus(runner({ status: "offline", draining: true }), missed))
      .toEqual({ label: "Offline", variant: "bad" });
  });

  it("names a status the server adds later instead of mislabelling it as critical", () => {
    expect(describeRunnerStatus(runner({ status: "quarantined" }), fresh))
      .toEqual({ label: "Quarantined", variant: "idle" });
    expect(describeRunnerStatus(runner({ status: "" }), fresh))
      .toEqual({ label: "Unknown", variant: "idle" });
  });
});

describe("countOnline", () => {
  it("counts only the runners the orchestrator can currently reach", () => {
    const runners = [
      runner({ id: "a" }),
      runner({ id: "b", status: "offline" }),
      runner({ id: "c", lastSeenAt: new Date(NOW - 10 * 60_000).toISOString() }),
      runner({ id: "d", lastSeenAt: "" }),
    ];
    expect(countOnline(runners, NOW)).toBe(1);
    expect(countOnline([], NOW)).toBe(0);
  });
});

describe("describeLoadError", () => {
  it("keeps the problem+json message the API sent", () => {
    expect(describeLoadError(new ApiError(503, "orchestrator unavailable: try again")))
      .toBe("orchestrator unavailable: try again");
  });

  it("explains the auth failures in terms of what to do next", () => {
    expect(describeLoadError(new ApiError(401, "Unauthorized"))).toMatch(/session has expired/);
    expect(describeLoadError(new ApiError(403, "Forbidden"))).toMatch(/not allowed/);
  });

  it("falls back for network failures and for values that are not errors", () => {
    expect(describeLoadError(new TypeError("Failed to fetch"))).toBe("Failed to fetch");
    expect(describeLoadError("nope")).toBe("The runner fleet could not be loaded.");
  });
});

describe("loadRunners", () => {
  it("returns the fleet on success", async () => {
    const api = { listRunners: async () => [runner()] } as unknown as ApiClient;
    expect(await loadRunners(api)).toEqual({ kind: "ready", runners: [runner()] });
  });

  it("turns a failure into an error result instead of rejecting or returning an empty fleet", async () => {
    const api = {
      listRunners: async () => {
        throw new ApiError(500, "orchestrator unavailable");
      },
    } as unknown as ApiClient;
    expect(await loadRunners(api)).toEqual({ kind: "error", message: "orchestrator unavailable" });
  });
});

describe("ApiClient.listRunners", () => {
  const okResponse = (body: unknown) =>
    new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });

  it("unwraps the runners array", async () => {
    const api = createApiClient(async () => okResponse({ runners: [runner()] }));
    await expect(api.listRunners()).resolves.toEqual([runner()]);
  });

  it("normalizes the null the Go handler emits for a runner with no labels", async () => {
    // encoding/json writes a nil []string as `null`, so an unlabelled runner
    // arrives with labels: null even though the OpenAPI schema says array.
    const api = createApiClient(async () => okResponse({ runners: [{ ...runner(), labels: null }] }));
    const [loaded] = await api.listRunners();
    expect(loaded.labels).toEqual([]);
  });

  it("requests the documented endpoint with the session cookie", async () => {
    const calls: Array<{ url: string; init?: RequestInit }> = [];
    const api = createApiClient(async (url, init) => {
      calls.push({ url: String(url), init });
      return okResponse({ runners: [] });
    });
    await api.listRunners();
    expect(calls[0].url).toBe("/api/v1/runners");
    expect(calls[0].init?.credentials).toBe("include");
  });

  it("forwards the abort signal it was given", async () => {
    const ctrl = new AbortController();
    let seen: AbortSignal | null | undefined;
    const api = createApiClient(async (_url, init) => {
      seen = init?.signal;
      return okResponse({ runners: [] });
    });
    await api.listRunners(ctrl.signal);
    expect(seen).toBe(ctrl.signal);
  });

  it("raises the problem+json detail instead of returning nothing", async () => {
    const api = createApiClient(async () =>
      new Response(JSON.stringify({ status: 503, title: "orchestrator unavailable", detail: "connection refused" }), {
        status: 503,
        headers: { "Content-Type": "application/problem+json" },
      })
    );
    await expect(api.listRunners()).rejects.toThrow("orchestrator unavailable: connection refused");
  });

  it("rejects a malformed body rather than reporting an empty fleet", async () => {
    const api = createApiClient(async () => okResponse({}));
    await expect(api.listRunners()).rejects.toThrow(/malformed/);
  });
});
