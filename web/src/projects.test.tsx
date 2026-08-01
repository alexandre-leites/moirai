// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { ProjectsPage } from "./projects";
import { button, buttons, chooseOption, click, field, form, selectField, submitForm, typeInto, unmountAll } from "./test-dom";
import { VIEWER, mountView, project, stubApi, workflow } from "./test-console";

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
    await submitForm(form(document.body));

    expect(createProject).toHaveBeenCalledWith({
      name: "payments-api",
      repositoryMode: "existing_path",
      repositoryUrl: undefined,
      localRepositoryPath: "/srv/repos/payments",
      defaultBranch: "main",
      requiredRunnerLabels: ["go", "docker"],
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
