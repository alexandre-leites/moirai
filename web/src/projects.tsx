// Projects (specification.md §5.6). Each project is one repository the Fates
// may work on — its issues, its labels, its delivery policy.
import { useCallback, useState, type FormEvent } from "react";
import { Link } from "react-router";
import type { ApiClient, CredentialKind, PipelineStep, Project, ProjectConfiguration, ProjectCredential } from "./api";
import { ApiError } from "./api";
import { activeWorkflowFor, useConsoleData } from "./console-data";
import { useIsAdmin } from "./auth";
import { ageAgo } from "./format";
import { usePolled } from "./poll";
import {
  Card, Empty, ErrorBlock, KV, KVRow, Modal, Pill, Skeleton, useConfirm, useToast,
} from "./ui";

export function ProjectsPage({ api }: { api: ApiClient }) {
  const isAdmin = useIsAdmin();
  const toast = useToast();
  const { data, error, loading, refresh } = useConsoleData();
  const [editing, setEditing] = useState<Project | null>(null);
  const [creating, setCreating] = useState(false);
  const [busyId, setBusyId] = useState<string | null>(null);

  const toggle = (project: Project) => {
    setBusyId(project.id);
    api.setProjectEnabled(project.id, !project.enabled).then(
      () => { toast(project.enabled ? "Scheduling paused for this project" : "Scheduling resumed"); refresh(); },
      (reason: unknown) => {
        toast(reason instanceof ApiError && reason.isForbidden
          ? "You need the admin role for that"
          : `Could not update ${project.name}: ${reason instanceof Error ? reason.message : String(reason)}`);
      }
    ).finally(() => setBusyId(null));
  };

  return (
    <div>
      <div className="view-head">
        <h1>Projects</h1>
        {isAdmin && (
          <button type="button" className="btn primary head-action" onClick={() => setCreating(true)}>
            Add project
          </button>
        )}
      </div>
      <p className="view-sub">
        Each project is one repository the orchestrator may work on — its issues, labels, pipeline, and
        merge policy.
      </p>

      {error && <ErrorBlock title={data ? "Showing the last good snapshot — the refresh failed." : "Projects could not be loaded."} detail={error} onRetry={refresh} />}
      {loading && !data && <Skeleton cards={2} />}

      {data && (data.projects.length === 0 ? (
        <Card><Empty>No project is registered yet. Add one to start scheduling work.</Empty></Card>
      ) : (
        <div className="grid cards-auto">
          {data.projects.map((project) => {
            const active = activeWorkflowFor(data.workflows, project.id);
            return (
              <Card key={project.id}>
                <div className="card-h">
                  <h2>{project.name}</h2>
                  {project.enabled ? <Pill variant="ok">Scheduling</Pill> : <Pill variant="idle">Paused</Pill>}
                </div>
                <div className="card-b">
                  <KV>
                    <KVRow label="Source">
                      <span className="num">
                        {project.repositoryMode === "existing_path" ? project.localRepositoryPath : project.repositoryUrl}
                      </span>
                    </KVRow>
                    <KVRow label="Mode">
                      {project.repositoryMode === "existing_path" ? "Existing path" : "Managed clone"}
                      {" · base "}<span className="num">{project.defaultBranch}</span>
                    </KVRow>
                    <KVRow label="Runner labels">
                      {project.requiredRunnerLabels.length === 0
                        ? <span className="t2">any runner</span>
                        : project.requiredRunnerLabels.map((label) => <Pill key={label} variant="idle" dot={false}>{label}</Pill>)}
                    </KVRow>
                    <KVRow label="Active thread">
                      {active
                        ? <Link className="num" to={`/workflows/${active.id}`}>{active.issueExternalId}</Link>
                        : <span className="t2">none</span>}
                    </KVRow>
                  </KV>
                  <Credentials api={api} projectId={project.id} />
                  {isAdmin && (
                    <div className="btnrow">
                      <button type="button" className="btn sm" onClick={() => setEditing(project)}>Configure</button>
                      <button type="button" className="btn sm" disabled={busyId === project.id} onClick={() => toggle(project)}>
                        {project.enabled ? "Pause scheduling" : "Resume scheduling"}
                      </button>
                    </div>
                  )}
                </div>
              </Card>
            );
          })}
        </div>
      ))}

      {creating && (
        <ProjectForm
          title="Add project"
          submitLabel="Add project"
          onClose={() => setCreating(false)}
          onSubmit={(config) => api.createProject(config)}
          onDone={() => { toast("Project added"); refresh(); }}
        />
      )}
      {editing && (
        <ProjectForm
          title="Configure project"
          submitLabel="Save changes"
          project={editing}
          onClose={() => setEditing(null)}
          onSubmit={(config) => api.updateProject(editing.id, config)}
          onDone={() => { toast("Project configuration saved"); refresh(); }}
        />
      )}
    </div>
  );
}

function ProjectForm({ title, submitLabel, project, onClose, onSubmit, onDone }: {
  title: string;
  submitLabel: string;
  project?: Project;
  onClose: () => void;
  onSubmit: (config: ProjectConfiguration) => Promise<Project>;
  onDone: () => void;
}) {
  const [name, setName] = useState(project?.name ?? "");
  const [mode, setMode] = useState(project?.repositoryMode === "existing_path" ? "existing_path" : "managed_clone");
  const [source, setSource] = useState(
    project ? (project.repositoryMode === "existing_path" ? project.localRepositoryPath : project.repositoryUrl) : ""
  );
  const [branch, setBranch] = useState(project?.defaultBranch ?? "main");
  const [labels, setLabels] = useState((project?.requiredRunnerLabels ?? []).join(", "));
  const [pipelineSteps, setPipelineSteps] = useState<PipelineStep[]>(project?.pipelineSteps ?? []);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!name.trim()) { setError("A project name is required."); return; }
    // Mode and source are exclusive: a managed clone needs a URL to clone from,
    // an existing-path project needs the path on the runner.
    if (!source.trim()) {
      setError(mode === "managed_clone" ? "A repository URL is required." : "A local repository path is required.");
      return;
    }
    setError("");
    setSaving(true);
    onSubmit({
      name: name.trim(),
      repositoryMode: mode,
      repositoryUrl: mode === "managed_clone" ? source.trim() : undefined,
      localRepositoryPath: mode === "existing_path" ? source.trim() : undefined,
      defaultBranch: branch.trim() || "main",
      requiredRunnerLabels: labels.split(",").map((label) => label.trim()).filter(Boolean),
      pipelineSteps: pipelineSteps.map((step, position) => ({ ...step, command: step.command.trim(), position })),
    }).then(
      () => { onDone(); onClose(); },
      (reason: unknown) => {
        setError(reason instanceof ApiError && reason.isForbidden
          ? "You need the admin role to change projects."
          : reason instanceof Error ? reason.message : "The project could not be saved.");
      }
    ).finally(() => setSaving(false));
  };

  return (
    <Modal title={title} onClose={onClose}>
      <h2>{title}</h2>
      <p>Repository, scheduling constraints, and delivery policy.</p>
      <form onSubmit={submit}>
        {error && <ErrorBlock title={error} />}
        <div className="form-grid">
          <label>
            Name
            <input value={name} placeholder="payments-api" onChange={(event) => setName(event.target.value)} />
          </label>
          <label>
            Mode
            <select value={mode} onChange={(event) => setMode(event.target.value)}>
              <option value="managed_clone">Managed clone</option>
              <option value="existing_path">Existing path</option>
            </select>
          </label>
          <label className="wide">
            {mode === "managed_clone" ? "Repository URL" : "Local repository path"}
            <input
              value={source}
              placeholder={mode === "managed_clone" ? "git@github.com:org/repository.git" : "/repositories/payments-api"}
              onChange={(event) => setSource(event.target.value)}
            />
          </label>
          <label>
            Base branch
            <input value={branch} onChange={(event) => setBranch(event.target.value)} />
          </label>
          <label>
            Runner labels
            <input value={labels} placeholder="go, docker" onChange={(event) => setLabels(event.target.value)} />
          </label>
          <div className="wide">
            <label>Pipeline steps</label>
            {pipelineSteps.map((step, index) => (
              <div className="btnrow" key={index}>
                <input aria-label={`Pipeline command ${index + 1}`} value={step.command} placeholder="make test" onChange={(event) => setPipelineSteps(pipelineSteps.map((current, i) => i === index ? { ...current, command: event.target.value } : current))} />
                <input aria-label={`Pipeline timeout ${index + 1}`} type="number" min="1" max="3600" value={step.timeoutSeconds} onChange={(event) => setPipelineSteps(pipelineSteps.map((current, i) => i === index ? { ...current, timeoutSeconds: Number(event.target.value) } : current))} />
                <label><input type="checkbox" checked={step.required} onChange={(event) => setPipelineSteps(pipelineSteps.map((current, i) => i === index ? { ...current, required: event.target.checked } : current))} /> Required</label>
                <button type="button" className="btn sm" onClick={() => setPipelineSteps(pipelineSteps.filter((_, i) => i !== index))}>Remove</button>
              </div>
            ))}
            <button type="button" className="btn sm" onClick={() => setPipelineSteps([...pipelineSteps, { command: "", timeoutSeconds: 600, position: pipelineSteps.length, required: true }])}>Add pipeline step</button>
          </div>
        </div>
        <div className="btnrow">
          <button type="submit" className="btn primary" disabled={saving}>{saving ? "Saving…" : submitLabel}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  );
}


const CREDENTIAL_LABELS: Record<CredentialKind, string> = {
  github_token: "GitHub token",
  ssh_private_key: "SSH private key",
};

const CREDENTIAL_HINTS: Record<CredentialKind, string> = {
  github_token: "Reads issues and opens pull requests as this token. Needs `repo` scope for a private repository.",
  ssh_private_key: "Used for git over SSH. Paste the private key, including its BEGIN and END lines.",
};

/**
 * Per-project credentials (specification: they replace the deployment-wide
 * token for this project).
 *
 * A value is never displayed, because none is ever returned — the API reports
 * only which kinds are configured and when they last changed. "Replace" is the
 * only edit; there is nothing to show and nothing to edit in place.
 */
function Credentials({ api, projectId }: { api: ApiClient; projectId: string }) {
  const isAdmin = useIsAdmin();
  const toast = useToast();
  const { confirm, dialog } = useConfirm();
  const [editing, setEditing] = useState<CredentialKind | null>(null);

  const load = useCallback(
    (signal: AbortSignal) => api.listProjectCredentials(projectId, signal),
    [api, projectId]
  );
  const { data, error, refresh } = usePolled(load, "Credentials are unavailable.", { poll: false });

  const configured = (kind: CredentialKind): ProjectCredential | undefined =>
    (data ?? []).find((entry) => entry.kind === kind);

  const clear = (kind: CredentialKind) => {
    api.clearProjectCredential(projectId, kind).then(
      () => { toast(`${CREDENTIAL_LABELS[kind]} removed`); refresh(); },
      (reason: unknown) => toast(credentialFailure(reason))
    );
  };

  if (error) return <p className="t2">{error}</p>;

  return (
    <div className="cred-list">
      {(Object.keys(CREDENTIAL_LABELS) as CredentialKind[]).map((kind) => {
        const entry = configured(kind);
        return (
          <div className="row-line" key={kind}>
            <span>
              {CREDENTIAL_LABELS[kind]}
              <span className="t2">
                {entry ? ` · set ${ageAgo(entry.updatedAt, "recently")}` : " · not set, uses the shared token"}
              </span>
            </span>
            {isAdmin && (
              <span className="btnrow">
                <button type="button" className="btn sm" onClick={() => setEditing(kind)}>
                  {entry ? "Replace" : "Set"}
                </button>
                {entry && (
                  <button
                    type="button"
                    className="btn sm danger"
                    onClick={() => confirm({
                      title: `Remove the ${CREDENTIAL_LABELS[kind].toLowerCase()}`,
                      body: "This project falls back to the deployment-wide credential. Work already running is unaffected.",
                      confirmLabel: "Remove",
                      danger: true,
                      onConfirm: () => clear(kind),
                    })}
                  >
                    Remove
                  </button>
                )}
              </span>
            )}
          </div>
        );
      })}
      {editing && (
        <CredentialForm
          api={api}
          projectId={projectId}
          kind={editing}
          onClose={() => setEditing(null)}
          onSaved={() => { toast(`${CREDENTIAL_LABELS[editing]} saved`); refresh(); }}
        />
      )}
      {dialog}
    </div>
  );
}

function credentialFailure(reason: unknown): string {
  if (reason instanceof ApiError && reason.isForbidden) return "You need the admin role for that";
  // Everything else is shown verbatim. A 422 carries the orchestrator's own
  // words, and the one an operator will actually hit -- "no secret key is
  // configured ... set LOOP_SECRET_KEY" -- names the fix. Replacing it with a
  // generic line here would strand them.
  return reason instanceof Error ? reason.message : "The credential could not be saved.";
}

function CredentialForm({ api, projectId, kind, onClose, onSaved }: {
  api: ApiClient;
  projectId: string;
  kind: CredentialKind;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const multiline = kind === "ssh_private_key";

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!value.trim()) { setError("Paste the credential, or cancel."); return; }
    setError("");
    setSaving(true);
    api.setProjectCredential(projectId, kind, value).then(
      () => { onSaved(); onClose(); },
      (reason: unknown) => setError(credentialFailure(reason))
    ).finally(() => setSaving(false));
  };

  return (
    <Modal title={CREDENTIAL_LABELS[kind]} onClose={onClose}>
      <h2>{CREDENTIAL_LABELS[kind]}</h2>
      <p>{CREDENTIAL_HINTS[kind]}</p>
      <form onSubmit={submit}>
        {error && <ErrorBlock title={error} />}
        <div className="form-grid">
          <label className="wide">
            Value
            {multiline ? (
              <textarea
                value={value}
                rows={6}
                aria-label={CREDENTIAL_LABELS[kind]}
                onChange={(event) => setValue(event.target.value)}
              />
            ) : (
              <input
                type="password"
                value={value}
                autoComplete="off"
                aria-label={CREDENTIAL_LABELS[kind]}
                onChange={(event) => setValue(event.target.value)}
              />
            )}
          </label>
        </div>
        <p className="t2">
          Stored encrypted. It is never shown again — replacing it is the only way to change it.
        </p>
        <div className="btnrow">
          <button type="submit" className="btn primary" disabled={saving}>{saving ? "Saving…" : "Save"}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  );
}
