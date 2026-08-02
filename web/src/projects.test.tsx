// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { ProjectsPage } from "./projects";
import { button, buttons, chooseOption, click, field, form, selectField, submitForm, textarea, typeInto, unmountAll } from "./test-dom";
import { VIEWER, credential, mountView, project, stubApi, workflow } from "./test-console";

afterEach(unmountAll);

describe("ProjectsPage", () => {
  it("shows a card per project with its source, mode and scheduling state", async () => {
    const api = stubApi({
      listProjects: async () => [
        project(),
        project({ id: "p2", name: "chronos", enabled: false, repositoryMode: "existing_path", localRepositoryPath: "/srv/repos/chronos", defaultBranch: "trunk" }),
      ],
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    expect(container.textContent).toContain("github.com/williamokano/moirai");
    expect(container.textContent).toContain("Scheduling");
    expect(container.textContent).toContain("/srv/repos/chronos");
    expect(container.textContent).toContain("Existing path");
    expect(container.textContent).toContain("Paused");
  });

  it("links the run currently holding a project's lock", async () => {
    const api = stubApi({
      listProjects: async () => [project()],
      listWorkflows: async () => [workflow({ id: "wf-9", projectId: "project-1", status: "implementing", issueExternalId: "#103" })],
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    const link = container.querySelector<HTMLAnchorElement>("a[href='/workflows/wf-9']")!;
    expect(link.textContent).toBe("#103");
  });

  it("says a project has no active thread rather than leaving the row blank", async () => {
    const api = stubApi({ listProjects: async () => [project()] });
    const container = await mountView(<ProjectsPage api={api} />, api);
    expect(container.textContent).toContain("none");
  });

  it("invites the first project when none is registered", async () => {
    const api = stubApi();
    const container = await mountView(<ProjectsPage api={api} />, api);
    expect(container.textContent).toContain("No project is registered yet");
  });

  it("pauses scheduling through the enable endpoint", async () => {
    const setProjectEnabled = vi.fn(async () => project({ enabled: false }));
    const api = stubApi({ listProjects: async () => [project()], setProjectEnabled });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(container, /Pause scheduling/));
    expect(setProjectEnabled).toHaveBeenCalledWith("project-1", false);
    expect(container.textContent).toContain("Scheduling paused");
  });

  it("creates a project from the form, mapping the mode to the right source field", async () => {
    const createProject = vi.fn(async () => project());
    const api = stubApi({ createProject });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(container, /Add project/));
    await typeInto(field(document.body, /Name/), "payments-api");
    await chooseOption(selectField(document.body, /Mode/), "existing_path");
    await typeInto(field(document.body, /Local repository path/), "/srv/repos/payments");
    await typeInto(field(document.body, /Runner labels/), "go, docker");
    await click(button(document.body, /Add pipeline step/));
    await typeInto(document.querySelector<HTMLInputElement>("input[aria-label='Pipeline command 1']")!, "go test ./...");
    await typeInto(document.querySelector<HTMLInputElement>("input[aria-label='Pipeline timeout 1']")!, "300");
    await submitForm(form(document.body));

    expect(createProject).toHaveBeenCalledWith({
      name: "payments-api",
      repositoryMode: "existing_path",
      repositoryUrl: undefined,
      localRepositoryPath: "/srv/repos/payments",
      defaultBranch: "main",
      requiredRunnerLabels: ["go", "docker"],
      pipelineSteps: [{ command: "go test ./...", timeoutSeconds: 300, position: 0, required: true }],
    });
  });

  it("refuses to submit a project with no source, and says which one it wants", async () => {
    const createProject = vi.fn(async () => project());
    const api = stubApi({ createProject });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(container, /Add project/));
    await typeInto(field(document.body, /Name/), "payments-api");
    await submitForm(form(document.body));

    expect(createProject).not.toHaveBeenCalled();
    expect(document.querySelector(".modal")?.textContent).toContain("A repository URL is required");
  });

  it("keeps the form open and explains a rejected save", async () => {
    const api = stubApi({ updateProject: async () => { throw new ApiError(403, "Forbidden"); }, listProjects: async () => [project()] });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(container, /Configure/));
    await submitForm(form(document.body));
    expect(document.querySelector(".modal")?.textContent).toContain("You need the admin role");
  });

  it("hides every mutating control from a viewer", async () => {
    const api = stubApi({ me: async () => VIEWER, listProjects: async () => [project()] });
    const container = await mountView(<ProjectsPage api={api} />, api);

    expect(buttons(container, /Add project|Configure|Pause scheduling/)).toHaveLength(0);
    expect(container.textContent).toContain("moirai");
  });

  it("surfaces a load failure", async () => {
    const api = stubApi({ listProjects: async () => { throw new Error("boom"); } });
    const container = await mountView(<ProjectsPage api={api} />, api);
    expect(container.textContent).toContain("boom");
  });
});

describe("project credentials", () => {
  /**
   * The row for one credential kind. Every kind gets a row whether or not it is
   * configured, so an unscoped `button(container, /^Set$/)` is ambiguous by
   * construction -- the tests have to say which credential they are acting on.
   */
  const credRow = (container: ParentNode, label: RegExp): HTMLElement => {
    const rows = Array.from(container.querySelectorAll<HTMLElement>(".cred-list > .row-line"))
      .filter((row) => label.test(row.textContent ?? ""));
    if (rows.length !== 1) throw new Error(`expected one credential row matching ${label}, found ${rows.length}`);
    return rows[0];
  };

  const modal = (): HTMLElement => {
    const found = document.querySelector<HTMLElement>(".modal");
    if (!found) throw new Error("no modal is open");
    return found;
  };

  it("shows which kinds are configured and when, without any value", async () => {
    const api = stubApi({
      listProjects: async () => [project()],
      listProjectCredentials: async () => [credential({ kind: "github_token" })],
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    expect(container.textContent).toContain("GitHub token");
    expect(container.textContent).toContain("set ");
    // The other kind is offered but reports the fallback rather than a blank.
    expect(container.textContent).toContain("SSH private key");
    expect(container.textContent).toContain("uses the shared token");
  });

  it("sends the typed value and never renders it back", async () => {
    const setProjectCredential = vi.fn(async () => [credential()]);
    const api = stubApi({ listProjects: async () => [project()], setProjectCredential });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(credRow(container, /GitHub token/), /^Set$/));
    const input = field(modal(), /Value/);
    expect(input.type).toBe("password");
    await typeInto(input, "ghp_a-real-token");
    await submitForm(form(modal()));

    expect(setProjectCredential).toHaveBeenCalledWith("project-1", "github_token", "ghp_a-real-token");
    // The list repaints from the reload, which reports presence only.
    expect(document.body.textContent).not.toContain("ghp_a-real-token");
    expect(container.querySelector(".modal")).toBeNull();
  });

  it("refuses to submit an empty credential", async () => {
    const setProjectCredential = vi.fn(async () => [credential()]);
    const api = stubApi({ listProjects: async () => [project()], setProjectCredential });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(credRow(container, /GitHub token/), /^Set$/));
    await submitForm(form(modal()));

    expect(setProjectCredential).not.toHaveBeenCalled();
    expect(modal().textContent).toContain("Paste the credential");
  });

  it("takes an SSH key as multi-line text rather than a single-line field", async () => {
    const setProjectCredential = vi.fn(async () => [credential({ kind: "ssh_private_key" })]);
    const api = stubApi({ listProjects: async () => [project()], setProjectCredential });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(credRow(container, /SSH private key/), /^Set$/));
    const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----";
    await typeInto(textarea(modal(), "SSH private key"), key);
    await submitForm(form(modal()));

    expect(setProjectCredential).toHaveBeenCalledWith("project-1", "ssh_private_key", key);
    expect(document.body.textContent).not.toContain("BEGIN OPENSSH");
  });

  it("names the consequence before removing one", async () => {
    const clearProjectCredential = vi.fn(async () => []);
    const api = stubApi({
      listProjects: async () => [project()],
      listProjectCredentials: async () => [credential()],
      clearProjectCredential,
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(credRow(container, /GitHub token/), /^Remove$/));
    expect(modal().textContent).toContain("falls back to the deployment-wide credential");
    expect(clearProjectCredential).not.toHaveBeenCalled();

    await click(button(modal(), /^Remove$/));
    expect(clearProjectCredential).toHaveBeenCalledWith("project-1", "github_token");
  });

  it("passes the deployment's missing-key error through verbatim", async () => {
    const detail = "no secret key is configured, so per-project credentials cannot be stored or read; set LOOP_SECRET_KEY";
    const api = stubApi({
      listProjects: async () => [project()],
      setProjectCredential: async () => { throw new ApiError(422, `Validation error: ${detail}`, detail); },
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(credRow(container, /GitHub token/), /^Set$/));
    await typeInto(field(modal(), /Value/), "ghp_x");
    await submitForm(form(modal()));

    expect(modal().textContent).toContain("LOOP_SECRET_KEY");
  });

  it("explains a rejected write without closing the form", async () => {
    const api = stubApi({
      listProjects: async () => [project()],
      setProjectCredential: async () => { throw new ApiError(403, "Forbidden"); },
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(credRow(container, /GitHub token/), /^Set$/));
    await typeInto(field(modal(), /Value/), "ghp_x");
    await submitForm(form(modal()));

    expect(modal().textContent).toContain("You need the admin role");
  });

  it("hides every credential control from a viewer", async () => {
    const api = stubApi({
      me: async () => VIEWER,
      listProjects: async () => [project()],
      listProjectCredentials: async () => [credential()],
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    expect(buttons(container, /^Set$|^Replace$|^Remove$/)).toHaveLength(0);
    // Still reports what is configured -- that is not a secret.
    expect(container.textContent).toContain("GitHub token");
  });
});
