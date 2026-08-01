// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { RunnersPage } from "./runners";
import { button, buttons, click, field, unmountAll, typeInto } from "./test-dom";
import { NOW, VIEWER, mountView, runner, stubApi, token } from "./test-console";

afterEach(unmountAll);

const HOURS_AGO = new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString();

describe("RunnersPage", () => {
  it("shows a card per runner with its heartbeat, labels and offer eligibility", async () => {
    const api = stubApi({ listRunners: async () => [runner(), runner({ id: "r2", name: "loom-02", draining: true })] });
    const container = await mountView(<RunnersPage api={api} />, api);

    expect(container.textContent).toContain("loom-01");
    expect(container.textContent).toContain("Online");
    expect(container.textContent).toContain("Draining");
    expect(container.textContent).toContain("docker");
    expect(container.textContent).toContain("finishes its current work");
  });

  it("reports a runner whose heartbeat has gone quiet as stale, not online", async () => {
    const api = stubApi({ listRunners: async () => [runner({ lastSeenAt: HOURS_AGO })] });
    const container = await mountView(<RunnersPage api={api} />, api);
    expect(container.textContent).toContain("Stale");
    expect(container.textContent).not.toContain("Online");
  });

  it("invites the operator to issue a token when the fleet is empty", async () => {
    const api = stubApi();
    const container = await mountView(<RunnersPage api={api} />, api);
    expect(container.textContent).toContain("No runner is registered");
  });

  it("surfaces a load failure instead of the empty fleet copy", async () => {
    const api = stubApi({ listRunners: async () => { throw new Error("orchestrator unreachable"); } });
    const container = await mountView(<RunnersPage api={api} />, api);
    expect(container.textContent).toContain("orchestrator unreachable");
    expect(container.textContent).not.toContain("No runner is registered");
  });

  it("drains a runner and reports what draining means", async () => {
    const setRunnerState = vi.fn(async () => runner({ draining: true }));
    const api = stubApi({ listRunners: async () => [runner()], setRunnerState });
    const container = await mountView(<RunnersPage api={api} />, api);

    await click(button(container, /^Drain$/));
    expect(setRunnerState).toHaveBeenCalledWith(runner().id, "drain");
    expect(container.textContent).toContain("then accepts no offers");
  });

  it("names the consequence before revoking a runner", async () => {
    const setRunnerState = vi.fn(async () => runner());
    const api = stubApi({ listRunners: async () => [runner()], setRunnerState });
    const container = await mountView(<RunnersPage api={api} />, api);

    await click(button(container, /^Revoke$/));
    expect(document.querySelector(".modal")?.textContent).toContain("credential is invalidated");
    expect(setRunnerState).not.toHaveBeenCalled();

    await click(button(document.body, /^Revoke runner$/));
    expect(setRunnerState).toHaveBeenCalledWith(runner().id, "revoke");
  });

  it("hides every mutating control from a viewer", async () => {
    const api = stubApi({ me: async () => VIEWER, listRunners: async () => [runner()], listTokens: async () => [token()] });
    const container = await mountView(<RunnersPage api={api} />, api);

    expect(buttons(container, /Drain|Revoke|New token/)).toHaveLength(0);
    expect(container.textContent).toContain("loom-01");
  });

  it("says a viewer needs the admin role rather than signing them out", async () => {
    const api = stubApi({
      listRunners: async () => [runner()],
      setRunnerState: async () => { throw new ApiError(403, "Forbidden"); },
    });
    const container = await mountView(<RunnersPage api={api} />, api);
    await click(button(container, /^Drain$/));
    expect(container.textContent).toContain("You need the admin role for that");
  });
});

describe("registration tokens", () => {
  it("lists tokens with their state and offers a revoke only for live ones", async () => {
    const api = stubApi({
      listTokens: async () => [
        token({ id: "tok-live" }),
        token({ id: "tok-used", usedAt: NOW }),
        token({ id: "tok-gone", revokedAt: NOW }),
      ],
    });
    const container = await mountView(<RunnersPage api={api} />, api);

    expect(container.textContent).toContain("consumed");
    expect(container.textContent).toContain("revoked");
    expect(buttons(container, /^Revoke$/).filter((btn) => btn.closest("td"))).toHaveLength(1);
  });

  it("shows a created token once, with a copy button and the warning", async () => {
    const createToken = vi.fn(async () => ({ token: "moi_secret_value", expiresAt: NOW }));
    const api = stubApi({ createToken });
    const container = await mountView(<RunnersPage api={api} />, api);

    await click(button(container, /New token/));
    await typeInto(field(document.body, /Allowed labels/), "go, docker");
    await click(button(document.body, /Create token/));

    expect(createToken).toHaveBeenCalledWith(["go", "docker"]);
    const modal = document.querySelector(".modal")!;
    expect(modal.textContent).toContain("moi_secret_value");
    expect(modal.textContent).toContain("shown only once");
    expect(button(modal, /Copy/)).toBeTruthy();
  });

  it("keeps showing tokens when the list request fails, with the reason", async () => {
    const api = stubApi({ listTokens: async () => { throw new Error("tokens unavailable"); } });
    const container = await mountView(<RunnersPage api={api} />, api);
    expect(container.textContent).toContain("tokens unavailable");
    expect(container.textContent).not.toContain("No registration token is outstanding");
  });
});
