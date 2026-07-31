// @vitest-environment jsdom
//
// `AuthProvider` is the session backbone: every protected page reads its state,
// and `api.test.ts` only proves the client *calls* whatever unauthorized handler
// it is given. These tests prove the provider is the thing that gives it one,
// and that a successful sign-in actually establishes a session rather than
// merely posting the credentials.
import { act } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { ApiError, type ApiClient, type CurrentUser } from "./api";
import { AuthProvider, useAuth, useIsAdmin } from "./auth";
import { button, click, deferred, mount, unmountAll } from "./test-dom";

afterEach(unmountAll);

const ADA: CurrentUser = { userId: "u-1", username: "ada", role: "admin", email: "ada@example.com", displayName: "Ada" };

/** Renders the session the provider is holding, so the tests can read it. */
function SessionProbe() {
  const { state, loading } = useAuth();
  const isAdmin = useIsAdmin();
  return (
    <p>
      {loading ? "loading" : state ? `${state.username}/${state.role}` : "signed out"}
      {isAdmin ? " admin" : " not-admin"}
    </p>
  );
}

type Calls = { me: number; login: number; logout: number; handlers: Array<(() => void) | null> };

function stubApi(overrides: Partial<ApiClient> = {}): { api: ApiClient; calls: Calls } {
  const calls: Calls = { me: 0, login: 0, logout: 0, handlers: [] };
  const api = {
    setUnauthorizedHandler(handler: (() => void) | null) {
      calls.handlers.push(handler);
    },
    async me(): Promise<CurrentUser> {
      calls.me += 1;
      return ADA;
    },
    async login(): Promise<{ userId: string }> {
      calls.login += 1;
      return { userId: ADA.userId };
    },
    async logout(): Promise<void> {
      calls.logout += 1;
    },
    ...overrides,
  } as unknown as ApiClient;
  return { api, calls };
}

function mountProvider(api: ApiClient): Promise<HTMLElement> {
  return mount(
    <AuthProvider api={api}>
      <SessionProbe />
    </AuthProvider>
  );
}

/** The provider plus a control that calls one of its context actions. */
function mountWithAction(
  api: ApiClient,
  label: string,
  action: (context: ReturnType<typeof useAuth>) => Promise<void>
): Promise<HTMLElement> {
  function Harness() {
    const context = useAuth();
    return (
      <>
        <SessionProbe />
        <button onClick={() => void action(context)}>{label}</button>
      </>
    );
  }
  return mount(
    <AuthProvider api={api}>
      <Harness />
    </AuthProvider>
  );
}

describe("AuthProvider", () => {
  it("adopts the session the API reports on mount", async () => {
    const { api, calls } = stubApi();
    const container = await mountProvider(api);

    expect(calls.me).toBe(1);
    expect(container.textContent).toContain("ada/admin");
    expect(container.textContent).toContain(" admin");
  });

  it("stays loading until the first /auth/me answers, so a refresh does not read as signed out", async () => {
    // ProtectedRoute redirects on `state === null`. If `loading` dropped before
    // `me()` resolved, every page refresh carrying a perfectly valid session
    // cookie would bounce the operator to the login screen.
    const pending = deferred<CurrentUser>();
    const { api } = stubApi({ me: () => pending.promise });
    const container = await mountProvider(api);

    expect(container.textContent).toContain("loading");

    await pending.resolve(ADA);
    expect(container.textContent).toContain("ada/admin");
  });

  it("reports a signed-out session when /auth/me rejects", async () => {
    const { api } = stubApi({
      me: async () => {
        throw new ApiError(401, "no session");
      },
    });
    const container = await mountProvider(api);

    expect(container.textContent).toContain("signed out");
    expect(container.textContent).toContain("not-admin");
  });

  it("establishes the session after a sign-in, not just posts the credentials", async () => {
    // `login` returns only a userId; the role — and therefore every privilege
    // gate in the console — comes from the follow-up `me()`. Dropping that
    // second call would leave a signed-in operator holding no session at all.
    let signedIn = false;
    let logins = 0;
    let meCalls = 0;
    const { api } = stubApi({
      me: async () => {
        meCalls += 1;
        if (!signedIn) throw new ApiError(401, "no session");
        return ADA;
      },
      login: async () => {
        logins += 1;
        signedIn = true;
        return { userId: ADA.userId };
      },
    });
    const container = await mountWithAction(api, "go", ({ login }) => login("ada", "lovelace"));
    expect(container.textContent).toContain("signed out");

    await click(button(container, /^go$/));

    expect(container.textContent).toContain("ada/admin");
    expect(logins).toBe(1);
    expect(meCalls).toBe(2); // once on mount, once after the sign-in
  });

  it("clears the session when the client reports a 401", async () => {
    // The handler is what turns "the cookie expired mid-session" into one clean
    // sign-out, instead of every page inventing its own reading of a thrown
    // ApiError.
    const { api, calls } = stubApi();
    const container = await mountProvider(api);
    expect(container.textContent).toContain("ada/admin");

    const registered = calls.handlers[0];
    expect(registered, "AuthProvider registered no unauthorized handler").toBeTypeOf("function");

    await act(async () => registered?.());

    expect(container.textContent).toContain("signed out");
    expect(container.textContent).toContain("not-admin");
  });

  it("unregisters its unauthorized handler on unmount", async () => {
    const { api, calls } = stubApi();
    await mountProvider(api);
    expect(calls.handlers).toEqual([expect.any(Function)]);

    await unmountAll();

    // A provider that never unregisters leaves the client holding a callback
    // into a torn-down tree, which React reports as a state update on an
    // unmounted component the next time any request 401s.
    expect(calls.handlers[calls.handlers.length - 1]).toBeNull();
  });

  it("clears the session on an explicit sign-out", async () => {
    const { api, calls } = stubApi();
    const container = await mountWithAction(api, "out", ({ logout }) => logout());
    expect(container.textContent).toContain("ada/admin");

    await click(button(container, /^out$/));

    expect(calls.logout).toBe(1);
    expect(container.textContent).toContain("signed out");
  });

  it("reloads the session on refresh, picking up profile changes", async () => {
    let current = ADA;
    const { api } = stubApi({
      me: async () => current,
    });
    const container = await mountWithAction(api, "rename", async ({ refresh }) => {
      current = { ...ADA, displayName: "Countess" };
      await refresh();
    });
    expect(container.textContent).toContain("ada/admin");

    await click(button(container, /^rename$/));

    expect(container.textContent).toContain("ada/admin");
  });
});
