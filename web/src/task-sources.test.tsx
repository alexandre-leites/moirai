// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { ProjectsPage } from "./projects";
import {
  button, chooseOption, click, field, form, selectField, submitForm, typeInto, unmountAll,
} from "./test-dom";
import { mountView, project, stubApi, taskSource, taskSourceTypes } from "./test-console";

afterEach(unmountAll);

function modal(): HTMLElement {
  const found = document.querySelector<HTMLElement>(".modal");
  if (!found) throw new Error("no modal is open");
  return found;
}

describe("TaskSources", () => {
  it("shows no task source configured until one is added", async () => {
    const api = stubApi({ listProjects: async () => [project()] });
    const container = await mountView(<ProjectsPage api={api} />, api);
    expect(container.textContent).toContain("No task source is configured");
  });

  it("lists a project's configured task sources with provider and state", async () => {
    const api = stubApi({
      listProjects: async () => [project({ taskSources: [taskSource({ name: "primary repo", enabled: true })] })],
    });
    const container = await mountView(<ProjectsPage api={api} />, api);
    expect(container.textContent).toContain("primary repo");
    expect(container.textContent).toContain("GitHub");
    expect(container.textContent).toContain("enabled");
  });

  // The whole point of #294/#345: a provider this test invents on the spot,
  // never seen anywhere in task-sources.tsx, must render correctly purely
  // from its descriptor. If this fails because the component special-cases
  // a provider id or field key, that is exactly the bug this test exists to
  // catch -- see the source file's own grep-for-literals self-review.
  it("renders a create form for a provider it has never seen before, purely from the descriptor", async () => {
    const thirdPartyType = {
      id: "totally_new_provider",
      displayName: "Totally New Provider",
      fields: [
        { key: "team_id", label: "Team ID", help: "", kind: "text" as const, required: true, options: [], pattern: "" },
        { key: "priority", label: "Priority", help: "", kind: "enum" as const, required: true, options: ["low", "high"], pattern: "" },
        { key: "max_items", label: "Max items", help: "", kind: "number" as const, required: false, options: [], pattern: "" },
        { key: "auto_sync", label: "Auto sync", help: "", kind: "bool" as const, required: false, options: [], pattern: "" },
        { key: "webhook_key", label: "Webhook key", help: "", kind: "secret" as const, required: false, options: [], pattern: "" },
      ],
    };
    const createTaskSource = vi.fn(async () => taskSource({ provider: "totally_new_provider" }));
    const api = stubApi({
      listProjects: async () => [project()],
      listTaskSourceTypes: async () => taskSourceTypes([thirdPartyType]),
      createTaskSource,
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(container, /Add task source/));
    await typeInto(field(container, /^Name/), "brand new source");
    // The provider select must offer exactly the descriptor's own type, with
    // no other option baked into the component.
    const providerSelect = selectField(container, /Provider/);
    expect(Array.from(providerSelect.options).map((o) => o.value)).toEqual(["totally_new_provider"]);
    await chooseOption(providerSelect, "totally_new_provider");

    await typeInto(field(container, /Team ID/), "team-9");
    await chooseOption(selectField(container, /Priority/), "high");
    await typeInto(field(container, /Max items/), "5");

    await submitForm(form(container));

    expect(createTaskSource).toHaveBeenCalledWith("project-1", expect.objectContaining({
      provider: "totally_new_provider",
      name: "brand new source",
      configuration: expect.objectContaining({ team_id: "team-9", priority: "high", max_items: 5, auto_sync: false }),
    }));
  });

  it("creates a task source through CreateTaskSource with the typed secret", async () => {
    const createTaskSource = vi.fn(async () => taskSource());
    const api = stubApi({
      listProjects: async () => [project()],
      listTaskSourceTypes: async () => taskSourceTypes(),
      createTaskSource,
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(container, /Add task source/));
    await typeInto(field(container, /^Name/), "primary");
    await typeInto(field(container, /Repository/), "acme/moirai");
    const secretInput = container.querySelector<HTMLInputElement>('input[aria-label="Personal access token"]')!;
    await typeInto(secretInput, "ghp_new_token");
    await submitForm(form(container));

    expect(createTaskSource).toHaveBeenCalledWith("project-1", {
      provider: "github",
      name: "primary",
      enabled: true,
      configuration: { ref: "acme/moirai" },
      secrets: { token: "ghp_new_token" },
    });
  });

  // Editing an unrelated field (name) must not blank a previously configured
  // secret: the frontend must omit the key entirely from `secrets`, not send
  // an empty string the backend could mistake for an explicit blank.
  it("editing an unrelated field never sends an empty string for an untouched secret", async () => {
    const updateTaskSource = vi.fn(async () => taskSource({ name: "renamed" }));
    const configured = taskSource({ name: "primary", secrets: [{ key: "token", configured: true }] });
    const api = stubApi({
      listProjects: async () => [project({ taskSources: [configured] })],
      listTaskSourceTypes: async () => taskSourceTypes(),
      updateTaskSource,
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(container, /Edit/));
    expect(container.textContent).toContain("configured");
    await typeInto(field(container, /^Name/), "renamed");
    await submitForm(form(container));

    expect(updateTaskSource).toHaveBeenCalledWith("ts-1", {
      name: "renamed",
      enabled: true,
      configuration: { ref: "acme/moirai" },
      secrets: {},
      clearSecrets: [],
    });
  });

  it("replaces a secret only when Replace is used and a value is typed", async () => {
    const updateTaskSource = vi.fn(async () => taskSource());
    const configured = taskSource({ secrets: [{ key: "token", configured: true }] });
    const api = stubApi({
      listProjects: async () => [project({ taskSources: [configured] })],
      listTaskSourceTypes: async () => taskSourceTypes(),
      updateTaskSource,
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(container, /Edit/));
    await click(button(container, /Replace/));
    const secretInput = container.querySelector<HTMLInputElement>('input[aria-label="Personal access token"]')!;
    await typeInto(secretInput, "ghp_replacement");
    await submitForm(form(container));

    expect(updateTaskSource).toHaveBeenCalledWith("ts-1", expect.objectContaining({
      secrets: { token: "ghp_replacement" },
      clearSecrets: [],
    }));
  });

  it("clears a secret only through the explicit Clear action", async () => {
    const updateTaskSource = vi.fn(async () => taskSource());
    const configured = taskSource({ secrets: [{ key: "token", configured: true }] });
    const api = stubApi({
      listProjects: async () => [project({ taskSources: [configured] })],
      listTaskSourceTypes: async () => taskSourceTypes(),
      updateTaskSource,
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(container, /Edit/));
    await click(button(container, /Clear/));
    expect(container.textContent).toContain("will be cleared on save");
    await submitForm(form(container));

    expect(updateTaskSource).toHaveBeenCalledWith("ts-1", expect.objectContaining({
      secrets: {},
      clearSecrets: ["token"],
    }));
  });

  it("deletes a task source after confirmation", async () => {
    const deleteTaskSource = vi.fn(async () => undefined);
    const configured = taskSource();
    const api = stubApi({
      listProjects: async () => [project({ taskSources: [configured] })],
      listTaskSourceTypes: async () => taskSourceTypes(),
      deleteTaskSource,
    });
    const container = await mountView(<ProjectsPage api={api} />, api);

    await click(button(container, /^Remove$/));
    await click(button(modal(), /^Remove$/));
    expect(deleteTaskSource).toHaveBeenCalledWith("ts-1");
  });
});
