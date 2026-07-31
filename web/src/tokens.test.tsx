// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { ApiError, type ApiClient, type CreatedToken, type RunnerToken } from "./api";
import { TokensPage } from "./tokens";
import {
  button,
  click,
  deferred,
  mount,
  unmountAll,
} from "./test-dom";

afterEach(unmountAll);

function token(overrides: Partial<RunnerToken> = {}): RunnerToken {
  return {
    id: "t-1",
    allowedLabels: [],
    expiresAt: "2027-01-01T00:00:00Z",
    ...overrides,
  };
}

type Stub = {
  listTokens?: ApiClient["listTokens"];
  createToken?: ApiClient["createToken"];
  revokeToken?: ApiClient["revokeToken"];
};

function stubApi(stub: Stub = {}) {
  const api = {
    setUnauthorizedHandler: () => undefined,
    listTokens: stub.listTokens ?? (async () => []),
    createToken: stub.createToken ?? (async () => ({ token: "tk-1", expiresAt: "2027-01-01T00:00:00Z" })),
    revokeToken: stub.revokeToken ?? (async () => undefined),
  } as unknown as ApiClient;
  return api;
}

describe("TokensPage revoke", () => {
  it("removes the token row on a successful 204 revoke", async () => {
    const api = stubApi({
      listTokens: async () => [token({ id: "t-1" }), token({ id: "t-2" })],
      revokeToken: async () => undefined,
    });
    const container = await mount(<TokensPage api={api} />);

    expect(container.querySelectorAll("tbody tr")).toHaveLength(2);
    const revokeBtns = container.querySelectorAll("tbody tr button");
    expect(revokeBtns).toHaveLength(2);
    await click(revokeBtns[0] as HTMLButtonElement);

    expect(container.querySelectorAll("tbody tr")).toHaveLength(1);
  });

  it("keeps the token row visible and reports failure on 404 revoke", async () => {
    const api = stubApi({
      listTokens: async () => [token({ id: "t-1", allowedLabels: ["linux"] })],
      revokeToken: async () => {
        throw new ApiError(404, "Not found");
      },
    });
    const container = await mount(<TokensPage api={api} />);

    expect(container.querySelectorAll("tbody tr")).toHaveLength(1);
    await click(button(container, /Revoke/));

    expect(container.querySelectorAll("tbody tr")).toHaveLength(1);
    expect(container.querySelector(".error")?.textContent).toContain("Not found");
  });

  it("keeps the token row visible and reports failure on 403 revoke", async () => {
    const api = stubApi({
      listTokens: async () => [token({ id: "t-1" })],
      revokeToken: async () => {
        throw new ApiError(403, "forbidden");
      },
    });
    const container = await mount(<TokensPage api={api} />);

    await click(button(container, /Revoke/));

    expect(container.querySelectorAll("tbody tr")).toHaveLength(1);
    expect(container.querySelector(".error")?.textContent).toContain("forbidden");
  });

  it("disables the revoke button while the request is pending", async () => {
    const pending = deferred<void>();
    const api = stubApi({
      listTokens: async () => [token({ id: "t-1" })],
      revokeToken: () => pending.promise,
    });
    const container = await mount(<TokensPage api={api} />);

    const revokeBtn = button(container, /Revoke/);
    expect(revokeBtn.disabled).toBe(false);

    await click(revokeBtn);
    const revokingBtn = button(container, /Revoking\.\.\./);
    expect(revokingBtn.disabled).toBe(true);

    await pending.resolve(undefined);
  });

  it("re-enables the revoke button when the request fails", async () => {
    const pending = deferred<void>();
    const api = stubApi({
      listTokens: async () => [token({ id: "t-1" })],
      revokeToken: () => pending.promise,
    });
    const container = await mount(<TokensPage api={api} />);

    await click(button(container, /Revoke/));
    expect(button(container, /Revoking\.\.\./).disabled).toBe(true);

    await pending.reject(new ApiError(500, "server error"));
    const retryBtn = button(container, /Revoke/);
    expect(retryBtn.disabled).toBe(false);
  });

  it("hides the revoke button for an already-revoked token", async () => {
    const api = stubApi({
      listTokens: async () => [token({ id: "t-1", revokedAt: "2026-07-30T00:00:00Z" })],
    });
    const container = await mount(<TokensPage api={api} />);

    expect(container.querySelectorAll("tbody tr")).toHaveLength(1);
    expect(container.textContent).not.toContain("Revoke");
  });

  it("hides the revoke button for a used token", async () => {
    const api = stubApi({
      listTokens: async () => [token({ id: "t-1", usedAt: "2026-07-30T00:00:00Z" })],
    });
    const container = await mount(<TokensPage api={api} />);

    expect(container.querySelectorAll("tbody tr")).toHaveLength(1);
    expect(container.textContent).not.toContain("Revoke");
  });
});

describe("TokensPage creation", () => {
  it("shows an error when token creation fails", async () => {
    const api = stubApi({
      listTokens: async () => [],
      createToken: async () => {
        throw new ApiError(500, "orchestrator unavailable: token generation failed");
      },
    });
    const container = await mount(<TokensPage api={api} />);

    await click(button(container, /Generate/));

    expect(container.textContent).toContain("orchestrator unavailable: token generation failed");
    expect(container.querySelector(".error")).not.toBeNull();
  });

  it("disables the generate button while creation is in flight", async () => {
    const pending = deferred<CreatedToken>();
    const api = stubApi({
      listTokens: async () => [],
      createToken: () => pending.promise,
    });
    const container = await mount(<TokensPage api={api} />);

    const genBtn = button(container, /Generate/);
    expect(genBtn.disabled).toBe(false);

    await click(genBtn);
    const generatingBtn = button(container, /Generating\.\.\./);
    expect(generatingBtn.disabled).toBe(true);

    await pending.resolve({ token: "tk-1", expiresAt: "2027-01-01T00:00:00Z" });
  });

  it("shows the created token after successful generation", async () => {
    const api = stubApi({
      listTokens: async () => [],
      createToken: async () => ({ token: "tk-secret-1", expiresAt: "2027-01-01T00:00:00Z" }),
    });
    const container = await mount(<TokensPage api={api} />);

    await click(button(container, /Generate/));

    expect(container.textContent).toContain("tk-secret-1");
    expect(container.textContent).toContain("This token is shown once");
  });
});
