import { describe, expect, it, vi } from "vitest";
import { ApiError, createApiClient } from "./api";

describe("API client", () => {
  it("clears auth only for 401 and sends the CSRF header", async () => {
    document.cookie = "loop_csrf=csrf-value";
    const fetcher = vi.fn(async () => new Response(JSON.stringify({ title: "Not authorized" }), { status: 403, headers: { "Content-Type": "application/json" } }));
    const client = createApiClient(fetcher);
    const unauthorized = vi.fn(); client.setUnauthorizedHandler(unauthorized);
    await expect(client.createToken(["linux"])).rejects.toMatchObject({ status: 403 } satisfies Partial<ApiError>);
    expect(unauthorized).not.toHaveBeenCalled();
    expect(fetcher).toHaveBeenCalledWith("/api/v1/runner-tokens", expect.objectContaining({ headers: expect.objectContaining({ "x-csrf-token": "csrf-value" }) }));
  });

  it("clears auth on 401", async () => {
    const client = createApiClient(async () => new Response(JSON.stringify({ title: "Unauthorized" }), { status: 401, headers: { "Content-Type": "application/json" } }));
    const unauthorized = vi.fn(); client.setUnauthorizedHandler(unauthorized);
    await expect(client.me()).rejects.toBeInstanceOf(ApiError);
    expect(unauthorized).toHaveBeenCalledOnce();
  });
});
