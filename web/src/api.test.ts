// @vitest-environment jsdom
//
// jsdom, not node: the CSRF token is read out of `document.cookie`, so the
// header assertions below need a document with a real cookie jar.
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, createApiClient } from "./api";

type Call = { url: string; init?: RequestInit };

/** The header value a request carried, or undefined when it sent no headers. */
function header(call: Call, name: string): string | undefined {
  const headers = call.init?.headers as Record<string, string> | undefined;
  return headers?.[name];
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function problemResponse(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/problem+json" },
  });
}

/** A fake fetch that records every call and answers with `respond`. */
function recorder(respond: (call: Call) => Response = () => jsonResponse({})) {
  const calls: Call[] = [];
  return {
    calls,
    fetchClient: async (url: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const call = { url: String(url), init };
      calls.push(call);
      return respond(call);
    },
  };
}

const setCookies: string[] = [];

function setCookie(name: string, value: string): void {
  setCookies.push(name);
  document.cookie = `${name}=${value}`;
}

afterEach(() => {
  for (const name of setCookies.splice(0)) {
    document.cookie = `${name}=; expires=Thu, 01 Jan 1970 00:00:00 GMT`;
  }
});

describe("createApiClient CSRF handling", () => {
  it("attaches the loop_csrf cookie to a mutation", async () => {
    // The cookie name is a contract with auth.CSRFCookieName in
    // api/internal/auth/session.go. Renaming the constant on either side
    // without the other silently strips the header and every mutation 403s.
    setCookie("loop_csrf", "token-abc");
    const { calls, fetchClient } = recorder(() => jsonResponse({ id: "p1", name: "svc", enabled: true }));
    await createApiClient(fetchClient).createProject({
      name: "svc",
      repositoryMode: "managed_clone",
      defaultBranch: "main",
    });
    expect(calls[0].init?.method).toBe("POST");
    expect(header(calls[0], "x-csrf-token")).toBe("token-abc");
  });

  it("attaches it to every mutating call, not just the one that was tested first", async () => {
    setCookie("loop_csrf", "token-abc");
    const { calls, fetchClient } = recorder(() => jsonResponse({ id: "x" }));
    const api = createApiClient(fetchClient);
    await api.logout();
    await api.createProject({ name: "svc", repositoryMode: "managed_clone", defaultBranch: "main" });
    await api.updateProject("p1", { name: "svc", repositoryMode: "managed_clone", defaultBranch: "main" });
    await api.setProjectEnabled("p1", false);
    await api.createToken(["linux"]);
    await api.revokeToken("t1");
    await api.submitWorkflowDecision("w1", "approved");

    expect(calls).toHaveLength(7);
    for (const call of calls) {
      expect(header(call, "x-csrf-token"), `${call.url} sent no CSRF header`).toBe("token-abc");
      expect(call.init?.credentials).toBe("include");
    }
  });

  it("sends no CSRF header at all when the cookie is absent", async () => {
    const { calls, fetchClient } = recorder(() => jsonResponse({ id: "p1" }));
    await createApiClient(fetchClient).setProjectEnabled("p1", true);
    expect(header(calls[0], "x-csrf-token")).toBeUndefined();
  });

  it("ignores a cookie whose name merely ends with the CSRF cookie name", async () => {
    // Without the `(?:^|;\s*)` anchor the regex would match inside
    // "xloop_csrf" and send the wrong token.
    setCookie("xloop_csrf", "impostor");
    setCookie("loop_csrf", "genuine");
    const { calls, fetchClient } = recorder(() => jsonResponse({ id: "p1" }));
    await createApiClient(fetchClient).setProjectEnabled("p1", true);
    expect(header(calls[0], "x-csrf-token")).toBe("genuine");
  });

  it("decodes a percent-encoded cookie value", async () => {
    setCookie("loop_csrf", "a%2Fb%2Bc");
    const { calls, fetchClient } = recorder(() => jsonResponse({ id: "p1" }));
    await createApiClient(fetchClient).setProjectEnabled("p1", true);
    expect(header(calls[0], "x-csrf-token")).toBe("a/b+c");
  });

  it("does not put the CSRF header on reads", async () => {
    setCookie("loop_csrf", "token-abc");
    const { calls, fetchClient } = recorder(() => jsonResponse({ projects: [] }));
    await createApiClient(fetchClient).listProjects();
    expect(header(calls[0], "x-csrf-token")).toBeUndefined();
  });

  it("does not put the CSRF header on login, which runs before any session exists", async () => {
    setCookie("loop_csrf", "stale-token");
    const { calls, fetchClient } = recorder(() => jsonResponse({ userId: "u1" }));
    await createApiClient(fetchClient).login("ada", "lovelace");
    expect(header(calls[0], "x-csrf-token")).toBeUndefined();
    expect(header(calls[0], "Content-Type")).toBe("application/json");
  });
});

describe("createApiClient unauthorized handling", () => {
  it("invokes the handler on a 401 and still rejects", async () => {
    const onUnauthorized = vi.fn();
    const { fetchClient } = recorder(() => problemResponse({ title: "session expired" }, 401));
    const api = createApiClient(fetchClient);
    api.setUnauthorizedHandler(onUnauthorized);

    await expect(api.me()).rejects.toBeInstanceOf(ApiError);
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it("leaves the handler alone for failures that are not a 401", async () => {
    const onUnauthorized = vi.fn();
    const { fetchClient } = recorder(() => problemResponse({ title: "forbidden" }, 403));
    const api = createApiClient(fetchClient);
    api.setUnauthorizedHandler(onUnauthorized);

    await expect(api.listProjects()).rejects.toThrow("forbidden");
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it("stops calling the handler once it is unregistered", async () => {
    const onUnauthorized = vi.fn();
    const { fetchClient } = recorder(() => problemResponse({ title: "session expired" }, 401));
    const api = createApiClient(fetchClient);
    api.setUnauthorizedHandler(onUnauthorized);
    await expect(api.me()).rejects.toThrow();

    api.setUnauthorizedHandler(null);
    await expect(api.me()).rejects.toThrow();
    expect(onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it("survives a 401 with no handler registered", async () => {
    const { fetchClient } = recorder(() => problemResponse({ title: "session expired" }, 401));
    await expect(createApiClient(fetchClient).me()).rejects.toThrow("session expired");
  });
});

describe("createApiClient error mapping", () => {
  it("turns a problem+json body into an ApiError carrying title and detail", async () => {
    const { fetchClient } = recorder(() =>
      problemResponse(
        {
          type: "about:blank",
          status: 409,
          title: "project already exists",
          detail: "a project named svc is already registered",
        },
        409
      )
    );
    const failure = await createApiClient(fetchClient)
      .createProject({ name: "svc", repositoryMode: "managed_clone", defaultBranch: "main" })
      .catch((err: unknown) => err);

    expect(failure).toBeInstanceOf(ApiError);
    const error = failure as ApiError;
    expect(error.name).toBe("ApiError");
    expect(error.status).toBe(409);
    expect(error.detail).toBe("a project named svc is already registered");
    expect(error.message).toBe("project already exists: a project named svc is already registered");
  });

  it("keeps the title alone when the problem body carries no detail", async () => {
    const { fetchClient } = recorder(() => problemResponse({ status: 503, title: "orchestrator unavailable" }, 503));
    const error = (await createApiClient(fetchClient)
      .listWorkflows()
      .catch((err: unknown) => err)) as ApiError;

    expect(error.message).toBe("orchestrator unavailable");
    expect(error.detail).toBeUndefined();
  });

  it("falls back to the status when the error body is not JSON", async () => {
    const { fetchClient } = recorder(() => new Response("<html>502 Bad Gateway</html>", { status: 502 }));
    const error = (await createApiClient(fetchClient)
      .listProjects()
      .catch((err: unknown) => err)) as ApiError;

    expect(error).toBeInstanceOf(ApiError);
    expect(error.status).toBe(502);
    expect(error.message).toBe("request failed: 502");
    expect(error.detail).toBeUndefined();
  });

  it("falls back to the status when the error body is empty", async () => {
    const { fetchClient } = recorder(() => new Response(null, { status: 500 }));
    const error = (await createApiClient(fetchClient)
      .listProjects()
      .catch((err: unknown) => err)) as ApiError;
    expect(error.message).toBe("request failed: 500");
  });

  it("ignores a title or detail that is not a string", async () => {
    // A non-conforming body must not put `[object Object]` in front of an operator.
    const { fetchClient } = recorder(() => jsonResponse({ title: 42, detail: { code: "x" } }, 500));
    const error = (await createApiClient(fetchClient)
      .listProjects()
      .catch((err: unknown) => err)) as ApiError;

    expect(error.message).toBe("request failed: 500");
    expect(error.detail).toBeUndefined();
  });

  it("ignores an empty-string detail rather than appending a bare colon", async () => {
    const { fetchClient } = recorder(() => problemResponse({ title: "conflict", detail: "" }, 409));
    const error = (await createApiClient(fetchClient)
      .listProjects()
      .catch((err: unknown) => err)) as ApiError;

    expect(error.message).toBe("conflict");
    expect(error.detail).toBeUndefined();
  });
});

describe("createApiClient request shapes", () => {
  it("posts the typed credentials to the login endpoint", async () => {
    const { calls, fetchClient } = recorder(() => jsonResponse({ userId: "u-1" }));
    await expect(createApiClient(fetchClient).login("ada", "lovelace")).resolves.toEqual({ userId: "u-1" });

    expect(calls[0].url).toBe("/api/v1/auth/login");
    expect(calls[0].init?.method).toBe("POST");
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ username: "ada", password: "lovelace" });
  });

  it("sends an empty comment rather than omitting the field when a decision has none", async () => {
    const { calls, fetchClient } = recorder(() => jsonResponse({ id: "w1", projectId: "p1", status: "running", phase: "implement" }));
    await createApiClient(fetchClient).submitWorkflowDecision("w1", "approved");

    expect(calls[0].url).toBe("/api/v1/workflows/w1/decision");
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ decision: "approved", comment: "" });
  });

  it("forwards the reviewer's comment with a changes_requested decision", async () => {
    const { calls, fetchClient } = recorder(() => jsonResponse({ id: "w1" }));
    await createApiClient(fetchClient).submitWorkflowDecision("w1", "changes_requested", "needs a test");

    expect(JSON.parse(String(calls[0].init?.body))).toEqual({
      decision: "changes_requested",
      comment: "needs a test",
    });
  });

  it("asks for an unrestricted token as an empty label list, not a missing field", async () => {
    const { calls, fetchClient } = recorder(() => jsonResponse({ token: "t", expiresAt: "2026-07-30T00:00:00Z" }));
    await createApiClient(fetchClient).createToken();
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ allowedLabels: [] });
  });

  it("routes enable and disable to their own endpoints", async () => {
    const { calls, fetchClient } = recorder(() => jsonResponse({ id: "p1", name: "svc", enabled: true }));
    const api = createApiClient(fetchClient);
    await api.setProjectEnabled("p1", true);
    await api.setProjectEnabled("p1", false);

    expect(calls[0].url).toBe("/api/v1/projects/p1/enable");
    expect(calls[1].url).toBe("/api/v1/projects/p1/disable");
  });

  it("unwraps the collection envelopes the API returns", async () => {
    const projects = [{ id: "p1", name: "svc", enabled: true }];
    const workflows = [{ id: "w1", projectId: "p1", status: "running", phase: "implement" }];
    const tokens = [{ id: "t1", allowedLabels: [], expiresAt: "2026-07-30T00:00:00Z" }];
    const api = createApiClient(async (url) => {
      if (String(url).startsWith("/api/v1/projects")) return jsonResponse({ projects });
      if (String(url).startsWith("/api/v1/workflows")) return jsonResponse({ workflows });
      return jsonResponse({ tokens });
    });

    await expect(api.listProjects()).resolves.toEqual(projects);
    await expect(api.listWorkflows()).resolves.toEqual(workflows);
    await expect(api.listTokens()).resolves.toEqual(tokens);
  });

  it("reports an unreachable API as unhealthy instead of throwing", async () => {
    const { fetchClient } = recorder(() => new Response(null, { status: 503 }));
    await expect(createApiClient(fetchClient).health()).resolves.toBe("unhealthy");
    await expect(createApiClient(async () => new Response(null, { status: 200 })).health()).resolves.toBe("healthy");
  });

  it("forwards the abort signal so a navigation cancels the request in flight", async () => {
    const controller = new AbortController();
    const { calls, fetchClient } = recorder(() => jsonResponse({ projects: [] }));
    await createApiClient(fetchClient).listProjects(controller.signal);
    expect(calls[0].init?.signal).toBe(controller.signal);
  });
});
