// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import type { ApiClient, CurrentUser } from "./api";
import { AuthProvider } from "./auth";
import { AccountPage } from "./account";
import { button, click, field, mount, typeInto, unmountAll } from "./test-dom";

afterEach(unmountAll);

const ADA: CurrentUser = { userId: "u-1", username: "ada", role: "admin", email: "ada@example.com", displayName: "Ada" };

function stubApi(overrides: Partial<ApiClient> = {}): { api: ApiClient; updated: Array<Record<string, string | undefined>> } {
  const updated: Array<Record<string, string | undefined>> = [];
  const api = {
    setUnauthorizedHandler: () => undefined,
    me: async (): Promise<CurrentUser> => ADA,
    updateAccount: async (data: Record<string, string | undefined>): Promise<CurrentUser> => {
      updated.push(data);
      return ADA;
    },
    ...overrides,
  } as unknown as ApiClient;
  return { api, updated };
}

async function mountAccount(api: ApiClient): Promise<HTMLElement> {
  return mount(
    <AuthProvider api={api}>
      <AccountPage api={api} />
    </AuthProvider>
  );
}

describe("AccountPage", () => {
  it("prefills the display name and email from the session", async () => {
    const { api } = stubApi();
    const container = await mountAccount(api);

    expect(field(container, /Display name/).value).toBe("Ada");
    expect(field(container, /Email/).value).toBe("ada@example.com");
  });

  it("submits profile fields without touching the password", async () => {
    const { api, updated } = stubApi();
    const container = await mountAccount(api);
    await typeInto(field(container, /Display name/), "Countess");
    await typeInto(field(container, /Email/), "countess@example.com");

    await click(button(container, /^Save changes$/));

    expect(updated).toEqual([
      { currentPassword: undefined, newPassword: undefined, newEmail: "countess@example.com", displayName: "Countess" },
    ]);
    expect(container.textContent).toContain("Account updated.");
  });

  it("requires matching confirmation before changing the password", async () => {
    const { api, updated } = stubApi();
    const container = await mountAccount(api);
    await typeInto(field(container, /Current password/), "old-secret");
    await typeInto(field(container, /New password/), "new-secret");
    await typeInto(field(container, /Confirm new password/), "different");

    await click(button(container, /^Save changes$/));

    expect(updated).toEqual([]);
    expect(container.textContent).toContain("New passwords do not match");
  });

  it("submits a password change with the current password", async () => {
    const { api, updated } = stubApi();
    const container = await mountAccount(api);
    await typeInto(field(container, /Current password/), "old-secret");
    await typeInto(field(container, /New password/), "new-secret");
    await typeInto(field(container, /Confirm new password/), "new-secret");

    await click(button(container, /^Save changes$/));

    expect(updated).toEqual([
      { currentPassword: "old-secret", newPassword: "new-secret", newEmail: "ada@example.com", displayName: "Ada" },
    ]);
  });

  it("surfaces API failures", async () => {
    const { api } = stubApi({
      updateAccount: async () => {
        throw new Error("current password is incorrect");
      },
    });
    const container = await mountAccount(api);
    await click(button(container, /^Save changes$/));

    expect(container.textContent).toContain("current password is incorrect");
  });
});
