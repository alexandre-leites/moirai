// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { ApiError, type ApiClient, type CurrentUser } from "./api";
import { AuthProvider } from "./auth";
import { LoginPage } from "./login";
import { button, deferred, field, form, mount, submitForm, typeInto, unmountAll } from "./test-dom";

afterEach(unmountAll);

type Attempt = { username: string; password: string };

/**
 * The page is mounted inside a real `AuthProvider` rather than a hand-rolled
 * context value, so the test exercises the wiring the operator actually uses:
 * LoginPage -> AuthProvider.login -> api.login followed by api.me.
 */
function authApi(onLogin?: (attempt: Attempt) => Promise<{ userId: string }>) {
  const attempts: Attempt[] = [];
  let signedIn = false;
  const api = {
    setUnauthorizedHandler: () => undefined,
    async me(): Promise<CurrentUser> {
      if (!signedIn) throw new ApiError(401, "no session");
      return { userId: "u-1", username: "ada", role: "admin" };
    },
    async login(username: string, password: string): Promise<{ userId: string }> {
      attempts.push({ username, password });
      const result = await (onLogin?.({ username, password }) ?? Promise.resolve({ userId: "u-1" }));
      signedIn = true;
      return result;
    },
  } as unknown as ApiClient;
  return { api, attempts };
}

async function mountLogin(api: ApiClient): Promise<HTMLElement> {
  return mount(
    <AuthProvider api={api}>
      <LoginPage />
    </AuthProvider>
  );
}

function signIn(container: HTMLElement): HTMLButtonElement {
  return button(container, /Sign(ing)? in/);
}

describe("LoginPage", () => {
  it("renders a sign-in form with a masked password field", async () => {
    const { api } = authApi();
    const container = await mountLogin(api);

    expect(container.textContent).toContain("Sign in");
    expect(field(container, /^Username/).type).toBe("text");
    expect(field(container, /^Password/).type).toBe("password");
    expect(field(container, /^Username/).autocomplete).toBe("username");
    expect(field(container, /^Password/).autocomplete).toBe("current-password");
    expect(signIn(container).disabled).toBe(false);
  });

  it("shows no error before the first submission", async () => {
    const { api } = authApi();
    const container = await mountLogin(api);
    expect(container.querySelector(".error")).toBeNull();
  });

  it("refuses an empty submission without calling the API", async () => {
    const { api, attempts } = authApi();
    const container = await mountLogin(api);

    await submitForm(form(container));

    expect(container.querySelector(".error")?.textContent).toBe("Username and password are required");
    expect(attempts).toEqual([]);
  });

  it("treats a whitespace-only username as missing", async () => {
    const { api, attempts } = authApi();
    const container = await mountLogin(api);

    await typeInto(field(container, /^Username/), "   ");
    await typeInto(field(container, /^Password/), "lovelace");
    await submitForm(form(container));

    expect(container.querySelector(".error")?.textContent).toBe("Username and password are required");
    expect(attempts).toEqual([]);
  });

  it("refuses a submission with a username but no password", async () => {
    const { api, attempts } = authApi();
    const container = await mountLogin(api);

    await typeInto(field(container, /^Username/), "ada");
    await submitForm(form(container));

    expect(container.querySelector(".error")?.textContent).toBe("Username and password are required");
    expect(attempts).toEqual([]);
  });

  it("sends exactly the credentials that were typed", async () => {
    const { api, attempts } = authApi();
    const container = await mountLogin(api);

    await typeInto(field(container, /^Username/), "ada");
    await typeInto(field(container, /^Password/), "lovelace");
    await submitForm(form(container));

    expect(attempts).toEqual([{ username: "ada", password: "lovelace" }]);
    expect(container.querySelector(".error")).toBeNull();
  });

  it("clears the validation error once the fields are filled in", async () => {
    const { api } = authApi();
    const container = await mountLogin(api);

    await submitForm(form(container));
    expect(container.querySelector(".error")).not.toBeNull();

    await typeInto(field(container, /^Username/), "ada");
    await typeInto(field(container, /^Password/), "lovelace");
    await submitForm(form(container));

    expect(container.querySelector(".error")).toBeNull();
  });

  it("reports a rejected sign-in and leaves the form usable for another try", async () => {
    const { api, attempts } = authApi(async () => {
      throw new ApiError(401, "invalid credentials");
    });
    const container = await mountLogin(api);

    await typeInto(field(container, /^Username/), "ada");
    await typeInto(field(container, /^Password/), "wrong");
    await submitForm(form(container));

    expect(container.querySelector(".error")?.textContent).toBe("Login failed. Check your credentials.");
    expect(signIn(container).disabled).toBe(false);
    expect(signIn(container).textContent).toBe("Sign in");
    expect(attempts).toHaveLength(1);

    // The failure message must not leak the raw server text to the operator,
    // and must not be mistaken for a success.
    expect(container.textContent).not.toContain("invalid credentials");
  });

  it("locks the form while the request is in flight and releases it afterwards", async () => {
    const pending = deferred<{ userId: string }>();
    const { api } = authApi(() => pending.promise);
    const container = await mountLogin(api);

    await typeInto(field(container, /^Username/), "ada");
    await typeInto(field(container, /^Password/), "lovelace");
    await submitForm(form(container));

    expect(signIn(container).textContent).toBe("Signing in...");
    expect(signIn(container).disabled).toBe(true);
    expect(field(container, /^Username/).disabled).toBe(true);
    expect(field(container, /^Password/).disabled).toBe(true);

    await pending.resolve({ userId: "u-1" });

    expect(signIn(container).textContent).toBe("Sign in");
    expect(signIn(container).disabled).toBe(false);
    expect(field(container, /^Username/).disabled).toBe(false);
  });

  it("releases the form when the request fails, so a retry is possible", async () => {
    const pending = deferred<{ userId: string }>();
    const { api } = authApi(() => pending.promise);
    const container = await mountLogin(api);

    await typeInto(field(container, /^Username/), "ada");
    await typeInto(field(container, /^Password/), "lovelace");
    await submitForm(form(container));
    expect(signIn(container).disabled).toBe(true);

    await pending.reject(new ApiError(503, "orchestrator unavailable"));

    expect(signIn(container).disabled).toBe(false);
    expect(container.querySelector(".error")?.textContent).toBe("Login failed. Check your credentials.");
  });
});
