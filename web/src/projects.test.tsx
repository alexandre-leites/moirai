// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { ApiError, type ApiClient, type CurrentUser, type Project } from "./api";
import { AuthProvider } from "./auth";
import { ProjectsPage } from "./projects";
import {
  button,
  buttons,
  chooseOption,
  click,
  deferred,
  field,
  form,
  mount,
  selectField,
  submitForm,
  typeInto,
  unmountAll,
} from "./test-dom";

afterEach(unmountAll);

type CreateData = Parameters<ApiClient["createProject"]>[0];
type UpdateData = Parameters<ApiClient["updateProject"]>[1];
type Toggle = { id: string; enabled: boolean };

function project(overrides: Partial<Project> = {}): Project {
  return {
    id: "p-1",
    name: "billing",
    enabled: true,
    repositoryMode: "managed_clone",
    repositoryUrl: "git@github.com:acme/billing.git",
    localRepositoryPath: "",
    defaultBranch: "main",
    requiredRunnerLabels: [],
    ...overrides,
  };
}

type Stub = {
  listProjects?: ApiClient["listProjects"];
  createProject?: ApiClient["createProject"];
  updateProject?: ApiClient["updateProject"];
  setProjectEnabled?: ApiClient["setProjectEnabled"];
};

function stubApi(stub: Stub, role = "admin") {
  const created: CreateData[] = [];
  const updated: UpdateData[] = [];
  const toggled: Toggle[] = [];
  const api = {
    setUnauthorizedHandler: () => undefined,
    me: async (): Promise<CurrentUser> => ({ userId: "u-1", username: "ada", role, email: "", displayName: "" }),
    listProjects: stub.listProjects ?? (async () => []),
    createProject:
      stub.createProject ??
      (async (data: CreateData) => {
        created.push(data);
        return project({ id: "p-new", name: data.name });
      }),
    updateProject:
      stub.updateProject ??
      (async (id: string, data: UpdateData) => {
        updated.push(data);
        return project({
          id,
          name: data.name,
          repositoryMode: data.repositoryMode,
          repositoryUrl: data.repositoryUrl ?? "",
          localRepositoryPath: data.localRepositoryPath ?? "",
          defaultBranch: data.defaultBranch,
          requiredRunnerLabels: data.requiredRunnerLabels ?? [],
        });
      }),
    setProjectEnabled:
      stub.setProjectEnabled ??
      (async (id: string, enabled: boolean) => {
        toggled.push({ id, enabled });
        return project({ id, name: "billing", enabled });
      }),
  } as unknown as ApiClient;
  return { api, created, updated, toggled };
}

/**
 * Mounts the page with no auth context at all — the default context value,
 * which is what renders while `GET /auth/me` is still in flight. This is *not*
 * a stand-in for a signed-in non-admin: that user has a session whose role is
 * simply not "admin", and the two are observably different. `mountWithSession`
 * with a non-admin role is the one that covers the privilege gate.
 */
function mountWithoutSession(api: ApiClient): Promise<HTMLElement> {
  return mount(<ProjectsPage api={api} />);
}

/**
 * Mounts the page inside a real `AuthProvider`, so the role comes from a
 * genuine `GET /auth/me` response (see `stubApi`'s `role` argument) rather than
 * from a hand-built context.
 */
function mountWithSession(api: ApiClient): Promise<HTMLElement> {
  return mount(
    <AuthProvider api={api}>
      <ProjectsPage api={api} />
    </AuthProvider>
  );
}

/** The `<tr>` in the table body that names `name`. */
function row(container: HTMLElement, name: string): HTMLTableRowElement {
  const found = Array.from(container.querySelectorAll("tbody tr")).find((candidate) =>
    candidate.querySelector("td")?.textContent === name
  );
  if (!found) throw new Error(`no row for "${name}" in:\n${container.innerHTML}`);
  return found as HTMLTableRowElement;
}

describe("ProjectsPage loading and listing", () => {
  it("shows a loading line before the first response, not an empty table", async () => {
    const pending = deferred<Project[]>();
    const { api } = stubApi({ listProjects: () => pending.promise });
    const container = await mountWithoutSession(api);

    expect(container.textContent).toContain("Loading projects...");
    expect(container.querySelector("table")).toBeNull();
    expect(container.textContent).not.toContain("No projects registered");

    await pending.resolve([]);
    expect(container.textContent).not.toContain("Loading projects...");
  });

  it("shows an explicit empty state when nothing is registered", async () => {
    const { api } = stubApi({ listProjects: async () => [] });
    const container = await mountWithoutSession(api);

    expect(container.textContent).toContain("No projects registered");
    expect(container.querySelector("table")).toBeNull();
  });

  it("renders one row per project with its enablement state", async () => {
    const { api } = stubApi({
      listProjects: async () => [
        project({ id: "p-1", name: "billing", enabled: true }),
        project({ id: "p-2", name: "ledger", enabled: false }),
      ],
    });
    const container = await mountWithoutSession(api);

    expect(container.querySelectorAll("tbody tr")).toHaveLength(2);
    expect(row(container, "billing").textContent).toContain("Enabled");
    expect(row(container, "ledger").textContent).toContain("Disabled");
  });

  it("renders the empty state when the load fails", async () => {
    // Documents today's behaviour, which is not the behaviour the design
    // package asks for: `projects.tsx` swallows the failure with
    // `.catch(() => undefined)`, so a dead orchestrator is indistinguishable
    // from an empty fleet of projects. docs/design/web-console/specification.md
    // §6 ("no silent `.catch(() => undefined)` anywhere") is the contract that
    // will change this; until then this test pins the real behaviour rather
    // than pretending an error state exists.
    const { api } = stubApi({
      listProjects: async () => {
        throw new ApiError(503, "orchestrator unavailable");
      },
    });
    const container = await mountWithoutSession(api);

    expect(container.textContent).toContain("No projects registered");
    expect(container.textContent).not.toContain("Loading projects...");
    expect(container.textContent).not.toContain("orchestrator unavailable");
  });
});

describe("ProjectsPage admin controls", () => {
  it("offers a signed-in non-admin no create or toggle control at all", async () => {
    // The privilege gate, tested against a real session whose role is not
    // "admin" — not against the absence of a session. `useIsAdmin` is
    // `state?.role === "admin"`; weakening it to `state !== null` would hand a
    // viewer the create form and the enable/disable toggles, and only this
    // test would notice.
    const { api } = stubApi({ listProjects: async () => [project()] }, "viewer");
    const container = await mountWithSession(api);

    expect(container.textContent).toContain("billing"); // the page did render
    expect(buttons(container, /New project/)).toHaveLength(0);
    expect(Array.from(container.querySelectorAll("th")).map((th) => th.textContent)).toEqual([
      "Name",
      "Status",
    ]);
    expect(container.querySelectorAll("button")).toHaveLength(0);
  });

  it("offers no controls while the session is still resolving", async () => {
    const { api } = stubApi({ listProjects: async () => [project()] });
    const container = await mountWithoutSession(api);

    expect(container.textContent).toContain("billing");
    expect(container.querySelectorAll("button")).toHaveLength(0);
    expect(container.textContent).not.toContain("Actions");
  });

  it("offers an admin the create control and a per-row toggle", async () => {
    const { api } = stubApi({ listProjects: async () => [project()] });
    const container = await mountWithSession(api);

    expect(button(container, /New project/).textContent).toBe("New project");
    expect(Array.from(container.querySelectorAll("th")).map((th) => th.textContent)).toEqual([
      "Name",
      "Status",
      "Actions",
    ]);
    expect(button(row(container, "billing"), /Disable/).textContent).toBe("Disable");
  });

  it("toggles a project through the API and renders the state the server returned", async () => {
    const { api, toggled } = stubApi({ listProjects: async () => [project({ enabled: true })] });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Disable/));

    expect(toggled).toEqual([{ id: "p-1", enabled: false }]);
    expect(row(container, "billing").textContent).toContain("Disabled");
    expect(button(row(container, "billing"), /Enable/).textContent).toBe("Enable");
  });

  it("disables the toggle while its request is in flight", async () => {
    const pending = deferred<Project>();
    const { api } = stubApi({
      listProjects: async () => [project()],
      setProjectEnabled: () => pending.promise,
    });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Disable/));
    expect(button(row(container, "billing"), /\.\.\./).disabled).toBe(true);

    await pending.resolve(project({ enabled: false }));
    expect(button(row(container, "billing"), /Enable/).disabled).toBe(false);
  });

  it("leaves the row unchanged when the toggle fails", async () => {
    const { api } = stubApi({
      listProjects: async () => [project({ enabled: true })],
      setProjectEnabled: async () => {
        throw new ApiError(403, "forbidden");
      },
    });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Disable/));

    expect(row(container, "billing").textContent).toContain("Enabled");
    expect(button(row(container, "billing"), /Disable/).disabled).toBe(false);
  });
});

describe("ProjectsPage creation", () => {
  async function openCreateForm(container: HTMLElement): Promise<HTMLFormElement> {
    await click(button(container, /New project/));
    return form(container);
  }

  it("opens and closes the create form from the same control", async () => {
    const { api } = stubApi({ listProjects: async () => [] });
    const container = await mountWithSession(api);

    expect(container.querySelector("form")).toBeNull();
    await click(button(container, /New project/));
    expect(container.querySelector("form")).not.toBeNull();
    expect(container.textContent).toContain("Create project");

    await click(button(container, /Cancel/));
    expect(container.querySelector("form")).toBeNull();
  });

  it("creates from the Create button, not only from a synthetic form event", async () => {
    // Every other test in this file dispatches `submit` at the <form>, which
    // succeeds even if the button is not a submit control. Clicking the real
    // button is what proves an operator can actually create a project:
    // changing `type="submit"` to `type="button"` leaves the form with no way
    // to submit at all, and only this test fails.
    const { api, created } = stubApi({ listProjects: async () => [] });
    const container = await mountWithSession(api);
    const createForm = await openCreateForm(container);

    await typeInto(field(createForm, /^Name/), "ledger");
    await click(button(createForm, /^Create$/));

    expect(created).toHaveLength(1);
    expect(created[0].name).toBe("ledger");
  });

  it("refuses to create a project with no name, without calling the API", async () => {
    const { api, created } = stubApi({ listProjects: async () => [] });
    const container = await mountWithSession(api);
    const createForm = await openCreateForm(container);

    await submitForm(createForm);

    expect(container.querySelector(".error")?.textContent).toBe("Project name is required");
    expect(created).toEqual([]);
  });

  it("sends a managed clone with its repository URL and trims the name", async () => {
    const { api, created } = stubApi({ listProjects: async () => [] });
    const container = await mountWithSession(api);
    const createForm = await openCreateForm(container);

    await typeInto(field(createForm, /^Name/), "  billing  ");
    await typeInto(field(createForm, /^Repository URL/), "git@github.com:acme/billing.git");
    await submitForm(createForm);

    expect(created).toEqual([
      {
        name: "billing",
        repositoryMode: "managed_clone",
        repositoryUrl: "git@github.com:acme/billing.git",
        localRepositoryPath: undefined,
        defaultBranch: "main",
        requiredRunnerLabels: [],
      },
    ]);
  });

  it("sends a local path instead of a URL once the mode is switched", async () => {
    const { api, created } = stubApi({ listProjects: async () => [] });
    const container = await mountWithSession(api);
    const createForm = await openCreateForm(container);

    await typeInto(field(createForm, /^Name/), "ledger");
    await chooseOption(selectField(createForm, /^Repository mode/), "existing_path");

    // The URL field is replaced by the path field, not merely ignored.
    expect(container.textContent).not.toContain("Repository URL");
    await typeInto(field(createForm, /^Local path/), "/repositories/ledger");
    await submitForm(createForm);

    expect(created[0].repositoryMode).toBe("existing_path");
    expect(created[0].localRepositoryPath).toBe("/repositories/ledger");
    expect(created[0].repositoryUrl).toBeUndefined();
  });

  it("splits the comma-separated runner labels and drops the blanks", async () => {
    const { api, created } = stubApi({ listProjects: async () => [] });
    const container = await mountWithSession(api);
    const createForm = await openCreateForm(container);

    await typeInto(field(createForm, /^Name/), "billing");
    await typeInto(field(createForm, /^Required runner labels/), " linux , ,docker, ");
    await submitForm(createForm);

    expect(created[0].requiredRunnerLabels).toEqual(["linux", "docker"]);
  });

  it("falls back to main when the default branch is cleared", async () => {
    const { api, created } = stubApi({ listProjects: async () => [] });
    const container = await mountWithSession(api);
    const createForm = await openCreateForm(container);

    await typeInto(field(createForm, /^Name/), "billing");
    await typeInto(field(createForm, /^Default branch/), "   ");
    await submitForm(createForm);

    expect(created[0].defaultBranch).toBe("main");
  });

  it("adds the created project to the table and closes the form", async () => {
    const { api } = stubApi({
      listProjects: async () => [project({ id: "p-1", name: "billing" })],
      createProject: async () => project({ id: "p-2", name: "ledger", enabled: false }),
    });
    const container = await mountWithSession(api);
    const createForm = await openCreateForm(container);

    await typeInto(field(createForm, /^Name/), "ledger");
    await submitForm(createForm);

    expect(container.querySelector("form")).toBeNull();
    expect(container.querySelectorAll("tbody tr")).toHaveLength(2);
    expect(row(container, "ledger").textContent).toContain("Disabled");
    expect(row(container, "billing").textContent).toContain("Enabled");
  });

  it("surfaces a failed creation in the form and keeps the operator's input", async () => {
    const { api } = stubApi({
      listProjects: async () => [],
      createProject: async () => {
        // Shaped exactly as `createApiClient` builds it from a problem+json
        // body, so the assertion below is about what an operator really reads.
        throw new ApiError(
          409,
          "project already exists: a project named ledger is already registered",
          "a project named ledger is already registered"
        );
      },
    });
    const container = await mountWithSession(api);
    const createForm = await openCreateForm(container);

    await typeInto(field(createForm, /^Name/), "ledger");
    await submitForm(createForm);

    expect(container.querySelector(".error")?.textContent).toBe(
      "project already exists: a project named ledger is already registered"
    );
    expect(container.querySelector("form")).not.toBeNull();
    expect(field(createForm, /^Name/).value).toBe("ledger");
    expect(container.querySelectorAll("tbody tr")).toHaveLength(0);
  });

  it("locks the submit control while the creation is in flight", async () => {
    const pending = deferred<Project>();
    const { api } = stubApi({ listProjects: async () => [], createProject: () => pending.promise });
    const container = await mountWithSession(api);
    const createForm = await openCreateForm(container);

    await typeInto(field(createForm, /^Name/), "ledger");
    await submitForm(createForm);

    expect(button(createForm, /Creating\.\.\./).disabled).toBe(true);
    expect(field(createForm, /^Name/).disabled).toBe(true);

    await pending.resolve(project({ id: "p-2", name: "ledger" }));
    expect(container.querySelector("form")).toBeNull();
  });
});

describe("ProjectsPage editing", () => {
  it("shows a signed-in non-admin no edit control at all", async () => {
    const { api } = stubApi({ listProjects: async () => [project()] }, "viewer");
    const container = await mountWithSession(api);

    expect(container.textContent).toContain("billing");
    expect(buttons(container, /Edit/)).toHaveLength(0);
  });

  it("offers an admin an edit control per row", async () => {
    const { api } = stubApi({
      listProjects: async () => [project(), project({ id: "p-2", name: "ledger" })],
    });
    const container = await mountWithSession(api);

    expect(buttons(container, /Edit/)).toHaveLength(2);
  });

  it("opens the edit form prefilled from the persisted configuration", async () => {
    const { api } = stubApi({
      listProjects: async () => [
        project({
          repositoryMode: "existing_path",
          repositoryUrl: "",
          localRepositoryPath: "/repositories/billing",
          defaultBranch: "trunk",
          requiredRunnerLabels: ["linux", "docker"],
        }),
      ],
    });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Edit/));
    const editForm = form(container);
    expect(editForm.textContent).toContain("Edit project");
    expect(field(editForm, /^Name/).value).toBe("billing");
    expect(selectField(editForm, /^Repository mode/).value).toBe("existing_path");
    expect(container.textContent).not.toContain("Repository URL");
    expect(field(editForm, /^Local path/).value).toBe("/repositories/billing");
    expect(field(editForm, /^Default branch/).value).toBe("trunk");
    expect(field(editForm, /^Required runner labels/).value).toBe("linux, docker");
  });

  it("prefills a managed-clone project with its URL", async () => {
    const { api } = stubApi({ listProjects: async () => [project()] });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Edit/));
    const editForm = form(container);
    expect(field(editForm, /^Repository URL/).value).toBe("git@github.com:acme/billing.git");
  });

  it("sends the updated configuration and replaces the row with the server result", async () => {
    const { api, updated } = stubApi({ listProjects: async () => [project()] });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Edit/));
    const editForm = form(container);
    await typeInto(field(editForm, /^Name/), "ledger");
    await typeInto(field(editForm, /^Repository URL/), "git@github.com:acme/ledger.git");
    await typeInto(field(editForm, /^Required runner labels/), "linux, docker");
    await submitForm(editForm);

    expect(updated).toEqual([
      {
        name: "ledger",
        repositoryMode: "managed_clone",
        repositoryUrl: "git@github.com:acme/ledger.git",
        localRepositoryPath: undefined,
        defaultBranch: "main",
        requiredRunnerLabels: ["linux", "docker"],
      },
    ]);
    expect(container.querySelector("form")).toBeNull();
    expect(row(container, "ledger").textContent).toContain("Enabled");
    expect(() => row(container, "billing")).toThrow();
  });

  it("switches a persisted managed-clone project to existing-path on save", async () => {
    const { api, updated } = stubApi({ listProjects: async () => [project()] });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Edit/));
    const editForm = form(container);
    await chooseOption(selectField(editForm, /^Repository mode/), "existing_path");
    await typeInto(field(editForm, /^Local path/), "/repositories/billing");
    await submitForm(editForm);

    expect(updated[0].repositoryMode).toBe("existing_path");
    expect(updated[0].localRepositoryPath).toBe("/repositories/billing");
    expect(updated[0].repositoryUrl).toBeUndefined();
  });

  it("refuses to save a blank name without calling the API", async () => {
    const { api, updated } = stubApi({ listProjects: async () => [project()] });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Edit/));
    const editForm = form(container);
    await typeInto(field(editForm, /^Name/), "   ");
    await submitForm(editForm);

    expect(container.querySelector(".error")?.textContent).toBe("Project name is required");
    expect(updated).toEqual([]);
    expect(container.querySelector("form")).not.toBeNull();
  });

  it("surfaces a failed update in the form and keeps the operator's input", async () => {
    const { api } = stubApi({
      listProjects: async () => [project()],
      updateProject: async () => {
        throw new ApiError(422, "Validation error: project configuration is invalid", "project configuration is invalid");
      },
    });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Edit/));
    const editForm = form(container);
    await typeInto(field(editForm, /^Name/), "ledger");
    await submitForm(editForm);

    expect(container.querySelector(".error")?.textContent).toBe(
      "Validation error: project configuration is invalid"
    );
    expect(container.querySelector("form")).not.toBeNull();
    expect(field(editForm, /^Name/).value).toBe("ledger");
  });

  it("locks the save control while the update is in flight", async () => {
    const pending = deferred<Project>();
    const { api } = stubApi({
      listProjects: async () => [project()],
      updateProject: () => pending.promise,
    });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Edit/));
    const editForm = form(container);
    await submitForm(editForm);

    expect(button(editForm, /Saving\.\.\./).disabled).toBe(true);
    expect(field(editForm, /^Name/).disabled).toBe(true);

    await pending.resolve(project({ name: "ledger" }));
    expect(container.querySelector("form")).toBeNull();
  });

  it("closes the edit form without saving from its own cancel control", async () => {
    const { api, updated } = stubApi({ listProjects: async () => [project()] });
    const container = await mountWithSession(api);

    await click(button(row(container, "billing"), /Edit/));
    const editForm = form(container);
    await click(button(editForm, /Cancel/));

    expect(container.querySelector("form")).toBeNull();
    expect(updated).toEqual([]);
  });
});
