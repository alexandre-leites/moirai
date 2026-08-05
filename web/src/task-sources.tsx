// Task sources (#294/#345): the generic console form for a project's
// configured sources of work (GitHub, a local file directory, or any future
// provider), rendered entirely from ListTaskSourceTypes' descriptor.
//
// Nothing in this file names a provider or a field key -- every input it
// draws comes from a TaskSourceField the discovery endpoint describes. That
// is the whole point of #294's descriptor: a third provider registered
// server-side needs zero changes here.
import { useState, type FormEvent } from "react";
import type {
  ApiClient, Project, TaskSource, TaskSourceCreate, TaskSourceField, TaskSourceTypeDescriptor, TaskSourceUpdate,
} from "./api";
import { ApiError } from "./api";
import { useIsAdmin } from "./auth-context";
import { useConsoleData } from "./console-data-context";
import { ErrorBlock, Modal } from "./ui";
import { useConfirm } from "./ui/use-confirm";
import { useToast } from "./ui/toast-context";

function taskSourceFailure(reason: unknown, fallback: string): string {
  if (reason instanceof ApiError && reason.isForbidden) return "You need the admin role for that";
  return reason instanceof Error ? reason.message : fallback;
}

/**
 * A project's configured task sources: list, create, edit, delete. `types`
 * is fetched once for the whole page (ProjectsPage) rather than per project
 * card, since the descriptor is not project-specific.
 */
export function TaskSources({ api, project, types, typesError }: {
  api: ApiClient;
  project: Project;
  types: TaskSourceTypeDescriptor[];
  typesError: string | null;
}) {
  const isAdmin = useIsAdmin();
  const toast = useToast();
  const { confirm, dialog } = useConfirm();
  const { refresh } = useConsoleData();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<TaskSource | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);

  const typeFor = (provider: string): TaskSourceTypeDescriptor | undefined =>
    types.find((type) => type.id === provider);

  const remove = (source: TaskSource) => {
    setBusyId(source.id);
    api.deleteTaskSource(source.id).then(
      () => { toast(`${source.name} removed`); refresh(); },
      (reason: unknown) => toast(taskSourceFailure(reason, "The task source could not be removed."))
    ).finally(() => setBusyId(null));
  };

  return (
    <div className="cred-list">
      <div className="row-line" style={{ borderBottom: "none", paddingBottom: 0 }}>
        <span>Task sources</span>
      </div>
      {typesError && <p className="t2">{typesError}</p>}
      {project.taskSources.length === 0 && <p className="t2">No task source is configured.</p>}
      {project.taskSources.map((source) => {
        const type = typeFor(source.provider);
        return (
          <div className="row-line" key={source.id}>
            <span>
              {source.name}
              <span className="t2">
                {" · "}{type?.displayName ?? source.provider}
                {" · "}{source.enabled ? "enabled" : "disabled"}
              </span>
            </span>
            {isAdmin && (
              <span className="btnrow">
                <button type="button" className="btn sm" disabled={busyId === source.id} onClick={() => setEditing(source)}>
                  Edit
                </button>
                <button
                  type="button"
                  className="btn sm danger"
                  disabled={busyId === source.id}
                  onClick={() => confirm({
                    title: `Remove ${source.name}`,
                    body: "This project stops reading tasks from this source. Work already running is unaffected.",
                    confirmLabel: "Remove",
                    danger: true,
                    onConfirm: () => remove(source),
                  })}
                >
                  Remove
                </button>
              </span>
            )}
          </div>
        );
      })}
      {isAdmin && (
        <div className="btnrow">
          <button type="button" className="btn sm" disabled={types.length === 0} onClick={() => setCreating(true)}>
            Add task source
          </button>
        </div>
      )}
      {creating && (
        <TaskSourceForm
          api={api}
          projectId={project.id}
          types={types}
          onClose={() => setCreating(false)}
          onDone={() => { toast("Task source added"); refresh(); }}
        />
      )}
      {editing && typeFor(editing.provider) && (
        <TaskSourceForm
          api={api}
          projectId={project.id}
          types={types}
          existing={editing}
          onClose={() => setEditing(null)}
          onDone={() => { toast("Task source saved"); refresh(); }}
        />
      )}
      {dialog}
    </div>
  );
}

type FieldValues = Record<string, string | number | boolean | string[]>;

/** The value a create form should pre-fill for one field, from its default. */
function initialValue(field: TaskSourceField, current?: unknown): string | number | boolean | string[] {
  const seed = current !== undefined ? current : field.defaultValue;
  switch (field.kind) {
    case "bool":
      return typeof seed === "boolean" ? seed : false;
    case "number":
      return typeof seed === "number" ? seed : "";
    case "string_list":
      return Array.isArray(seed) ? seed.filter((entry): entry is string => typeof entry === "string") : [];
    default:
      return typeof seed === "string" ? seed : "";
  }
}

/** Mirrors validateFieldValue in orchestrator/internal/server/descriptor.go, for UX only -- the server is the source of truth. */
function fieldError(field: TaskSourceField, value: string | number | boolean | string[]): string | null {
  if (field.kind === "text" || field.kind === "enum") {
    const text = typeof value === "string" ? value.trim() : "";
    if (field.required && text === "") return `${field.label} is required.`;
    if (text === "") return null;
    if (field.kind === "enum" && field.options.length > 0 && !field.options.includes(text)) {
      return `${field.label} must be one of ${field.options.join(", ")}.`;
    }
    if (field.kind === "text" && field.pattern) {
      try {
        if (!new RegExp(field.pattern).test(text)) return `${field.label} does not match the required format.`;
      } catch {
        // An unparsable pattern is a server-side descriptor bug, not something the console can validate against.
      }
    }
    return null;
  }
  if (field.kind === "number" && field.required && value === "") return `${field.label} is required.`;
  return null;
}

function configurationValue(field: TaskSourceField, value: string | number | boolean | string[]): unknown {
  if (field.kind === "text" || field.kind === "enum") return typeof value === "string" ? value.trim() : value;
  return value;
}

/**
 * Create or edit one task source, every field rendered from `type.fields`.
 * `existing` present means edit: the provider is fixed to what it already
 * is, and secret fields render "configured"/"not configured" plus Replace
 * (and Clear, for one already configured) instead of a value -- the server
 * never returns one to render.
 */
function TaskSourceForm({ api, projectId, types, existing, onClose, onDone }: {
  api: ApiClient;
  projectId: string;
  types: TaskSourceTypeDescriptor[];
  existing?: TaskSource;
  onClose: () => void;
  onDone: () => void;
}) {
  const [provider, setProvider] = useState(existing?.provider ?? types[0]?.id ?? "");
  const type = types.find((candidate) => candidate.id === provider);
  const nonSecretFields = (type?.fields ?? []).filter((field) => field.kind !== "secret");
  const secretFields = (type?.fields ?? []).filter((field) => field.kind === "secret");

  const [name, setName] = useState(existing?.name ?? "");
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const [values, setValues] = useState<FieldValues>(() => {
    const initial: FieldValues = {};
    for (const field of nonSecretFields) initial[field.key] = initialValue(field, existing?.configuration[field.key]);
    return initial;
  });
  // What the operator typed to replace a secret; an empty entry (the
  // default, and what a field the operator never touched stays at) means
  // "leave unchanged" and must never be sent as an empty string.
  const [secretInputs, setSecretInputs] = useState<Record<string, string>>({});
  const [replacing, setReplacing] = useState<Record<string, boolean>>({});
  const [clearing, setClearing] = useState<Record<string, boolean>>({});
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  const configuredSecret = (key: string): boolean =>
    existing?.secrets.find((secret) => secret.key === key)?.configured ?? false;

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!name.trim()) { setError("A name is required."); return; }
    if (!type) { setError("Choose a provider."); return; }
    for (const field of nonSecretFields) {
      const problem = fieldError(field, values[field.key]);
      if (problem) { setError(problem); return; }
    }
    for (const field of secretFields) {
      const typed = secretInputs[field.key]?.trim() ?? "";
      const willBeConfigured = existing
        ? (configuredSecret(field.key) && !clearing[field.key]) || typed !== ""
        : typed !== "";
      if (field.required && !willBeConfigured) { setError(`${field.label} is required.`); return; }
    }
    setError("");
    setSaving(true);

    const configuration: Record<string, unknown> = {};
    for (const field of nonSecretFields) configuration[field.key] = configurationValue(field, values[field.key]);
    const secrets: Record<string, string> = {};
    for (const field of secretFields) {
      const typed = secretInputs[field.key]?.trim() ?? "";
      if (typed !== "") secrets[field.key] = typed;
    }

    const request = existing
      ? api.updateTaskSource(existing.id, {
          name: name.trim(), enabled, configuration,
          secrets, clearSecrets: secretFields.filter((field) => clearing[field.key]).map((field) => field.key),
        } satisfies TaskSourceUpdate)
      : api.createTaskSource(projectId, {
          provider: type.id, name: name.trim(), enabled, configuration, secrets,
        } satisfies TaskSourceCreate);

    request.then(
      () => { onDone(); onClose(); },
      (reason: unknown) => setError(taskSourceFailure(reason, "The task source could not be saved."))
    ).finally(() => setSaving(false));
  };

  const setValue = (key: string, value: string | number | boolean | string[]) =>
    setValues((current) => ({ ...current, [key]: value }));

  const renderField = (field: TaskSourceField) => {
    const value = values[field.key];
    const label = <>{field.label}{field.required && " *"}</>;
    switch (field.kind) {
      case "bool":
        return (
          <label key={field.key}>
            <input type="checkbox" checked={value === true} onChange={(event) => setValue(field.key, event.target.checked)} />
            {" "}{label}
          </label>
        );
      case "enum":
        return (
          <label key={field.key}>
            {label}
            <select value={typeof value === "string" ? value : ""} onChange={(event) => setValue(field.key, event.target.value)}>
              <option value="">Choose…</option>
              {field.options.map((option) => <option key={option} value={option}>{option}</option>)}
            </select>
            {field.help && <span className="t2">{field.help}</span>}
          </label>
        );
      case "number":
        return (
          <label key={field.key}>
            {label}
            <input
              type="number"
              value={typeof value === "number" ? value : ""}
              onChange={(event) => setValue(field.key, event.target.value === "" ? "" : Number(event.target.value))}
            />
            {field.help && <span className="t2">{field.help}</span>}
          </label>
        );
      case "string_list":
        return (
          <label key={field.key} className="wide">
            {label}
            <input
              value={Array.isArray(value) ? value.join(", ") : ""}
              placeholder="one, two, three"
              onChange={(event) => setValue(field.key, event.target.value.split(",").map((entry) => entry.trim()).filter(Boolean))}
            />
            {field.help && <span className="t2">{field.help}</span>}
          </label>
        );
      default:
        return (
          <label key={field.key} className="wide">
            {label}
            <input value={typeof value === "string" ? value : ""} onChange={(event) => setValue(field.key, event.target.value)} />
            {field.help && <span className="t2">{field.help}</span>}
          </label>
        );
    }
  };

  const renderSecretField = (field: TaskSourceField) => {
    const configured = configuredSecret(field.key);
    const isClearing = clearing[field.key] ?? false;
    const isReplacing = replacing[field.key] ?? false;
    return (
      <div className="row-line" key={field.key}>
        <span>
          {field.label}{field.required && !configured && !existing && " *"}
          <span className="t2">
            {existing
              ? isClearing ? " · will be cleared on save" : configured ? " · configured" : " · not configured"
              : ""}
          </span>
        </span>
        <span className="btnrow">
          {(!existing || isReplacing) ? (
            <input
              type="password"
              autoComplete="off"
              aria-label={field.label}
              placeholder={field.help || undefined}
              value={secretInputs[field.key] ?? ""}
              onChange={(event) => setSecretInputs((current) => ({ ...current, [field.key]: event.target.value }))}
            />
          ) : (
            <button type="button" className="btn sm" onClick={() => setReplacing((current) => ({ ...current, [field.key]: true }))}>
              {configured ? "Replace" : "Set"}
            </button>
          )}
          {existing && configured && !isClearing && (
            <button
              type="button"
              className="btn sm danger"
              onClick={() => {
                setClearing((current) => ({ ...current, [field.key]: true }));
                setReplacing((current) => ({ ...current, [field.key]: false }));
                setSecretInputs((current) => ({ ...current, [field.key]: "" }));
              }}
            >
              Clear
            </button>
          )}
          {existing && isClearing && (
            <button type="button" className="btn sm" onClick={() => setClearing((current) => ({ ...current, [field.key]: false }))}>
              Undo
            </button>
          )}
        </span>
      </div>
    );
  };

  return (
    <Modal title={existing ? `Edit ${existing.name}` : "Add task source"} onClose={onClose}>
      <h2>{existing ? `Edit ${existing.name}` : "Add task source"}</h2>
      <p>Rendered from the provider's own field descriptor.</p>
      <form onSubmit={submit}>
        {error && <ErrorBlock title={error} />}
        <div className="form-grid">
          <label>
            Name
            <input value={name} placeholder="Primary repository" onChange={(event) => setName(event.target.value)} />
          </label>
          <label>
            Provider
            {existing ? (
              <input value={type?.displayName ?? existing.provider} disabled />
            ) : (
              <select value={provider} onChange={(event) => setProvider(event.target.value)}>
                {types.map((candidate) => <option key={candidate.id} value={candidate.id}>{candidate.displayName}</option>)}
              </select>
            )}
          </label>
          <label>
            <input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
            {" "}Enabled
          </label>
          {nonSecretFields.map(renderField)}
        </div>
        {secretFields.length > 0 && (
          <fieldset>
            <legend>Secrets</legend>
            {secretFields.map(renderSecretField)}
          </fieldset>
        )}
        <div className="btnrow">
          <button type="submit" className="btn primary" disabled={saving || !type}>
            {saving ? "Saving…" : existing ? "Save changes" : "Add task source"}
          </button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  );
}
