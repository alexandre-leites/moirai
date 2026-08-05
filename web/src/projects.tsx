// Projects (specification.md §5.6). Each project is one repository the Fates
// may work on — its issues, its labels, its delivery policy.
import { useCallback, useState, type FormEvent } from "react";
import { Link } from "react-router";
import {
  AGENT_CREDENTIAL_NAME, AGENT_CREDENTIAL_PREFIX, agentCredentialName, isAgentCredentialKind,
} from "./api";
import type {
  ApiClient, CredentialKind, GitCredentialKind, PipelineStep, Project, ProjectConfiguration, ProjectCredential,
} from "./api";
import { ApiError } from "./api";
import { activeWorkflowFor, useConsoleData } from "./console-data-context";
import { useIsAdmin } from "./auth-context";
import { ageAgo } from "./format";
import { usePolled } from "./poll";
import { TaskSources } from "./task-sources";
import {
  Card, Empty, ErrorBlock, KV, KVRow, Modal, Pill, Skeleton,
} from "./ui";
import { useConfirm } from "./ui/use-confirm";
import { useToast } from "./ui/toast-context";

export function ProjectsPage({ api }: { api: ApiClient }) {
  const isAdmin = useIsAdmin();
  const toast = useToast();
  const { data, error, loading, refresh } = useConsoleData();
  const [editing, setEditing] = useState<Project | null>(null);
  const [creating, setCreating] = useState(false);
  const [busyId, setBusyId] = useState<string | null>(null);

  // The task source type descriptor is not project-specific, so it is loaded
  // once for the whole page rather than once per project card.
  const loadTaskSourceTypes = useCallback((signal: AbortSignal) => api.listTaskSourceTypes(signal), [api]);
  const taskSourceTypes = usePolled(loadTaskSourceTypes, "Task source types are unavailable.", { poll: false });

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
                  <TaskSources
                    api={api}
                    project={project}
                    types={taskSourceTypes.data ?? []}
                    typesError={taskSourceTypes.error}
                  />
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
  const [pipelineSteps, setPipelineSteps] = useState(project?.pipelineSteps ?? []);
  const [executionImage, setExecutionImage] = useState(project?.executionImage ?? "");
  const [requireHumanApproval, setRequireHumanApproval] = useState(project?.requireHumanApproval ?? false);
  const [requirePlanning, setRequirePlanning] = useState(project?.requirePlanning ?? false);
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
    if (pipelineSteps.some((step) => !step.command.trim() || !Number.isInteger(step.timeoutSeconds) || step.timeoutSeconds < 1)) {
      setError("Each pipeline command needs a command and positive timeout.");
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
      pipelineSteps: pipelineSteps.map((step, position) => ({ ...step, position })),
      executionImage: executionImage.trim() || undefined,
      requireHumanApproval: requireHumanApproval || undefined,
      requirePlanning: requirePlanning || undefined,
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
          <label>
            Execution image
            <input value={executionImage} placeholder="ghcr.io/acme/project-tools:latest" onChange={(event) => setExecutionImage(event.target.value)} />
          </label>
        </div>
        <fieldset>
          <legend>Local pipeline</legend>
          <p className="t2">Required commands must pass before AI review. No required command blocks completion.</p>
          {pipelineSteps.map((step, index) => (
            <div className="row-line" key={`${step.position}-${index}`}>
              <input
                aria-label={`Pipeline command ${index + 1}`}
                value={step.command}
                placeholder="make test"
                onChange={(event) => setPipelineSteps(pipelineSteps.map((item, i) => i === index ? { ...item, command: event.target.value } : item))}
              />
              <input
                aria-label={`Pipeline timeout ${index + 1}`}
                type="number"
                min="1"
                value={step.timeoutSeconds}
                onChange={(event) => setPipelineSteps(pipelineSteps.map((item, i) => i === index ? { ...item, timeoutSeconds: Number(event.target.value) } : item))}
              />
              <label>
                <input
                  type="checkbox"
                  checked={step.required}
                  onChange={(event) => setPipelineSteps(pipelineSteps.map((item, i) => i === index ? { ...item, required: event.target.checked } : item))}
                /> Required
              </label>
              <button type="button" className="btn sm" disabled={index === 0} onClick={() => setPipelineSteps(movePipelineStep(pipelineSteps, index, -1))}>Up</button>
              <button type="button" className="btn sm" disabled={index === pipelineSteps.length - 1} onClick={() => setPipelineSteps(movePipelineStep(pipelineSteps, index, 1))}>Down</button>
              <button type="button" className="btn sm danger" onClick={() => setPipelineSteps(pipelineSteps.filter((_, i) => i !== index))}>Remove</button>
            </div>
          ))}
          <button type="button" className="btn sm" onClick={() => setPipelineSteps([...pipelineSteps, newPipelineStep(pipelineSteps.length)])}>Add command</button>
        </fieldset>
        <fieldset>
          <legend>Delivery policy</legend>
          <label>
            <input
              type="checkbox"
              checked={requirePlanning}
              onChange={(event) => setRequirePlanning(event.target.checked)}
            /> Plan before implementing
          </label>
          <p className="t2">
            When checked, a planner execution runs before the developer one and its output is carried into the
            developer packet's context; the run sits at "Planning" while it does. Off by default.
          </p>
          <label>
            <input
              type="checkbox"
              checked={requireHumanApproval}
              onChange={(event) => setRequireHumanApproval(event.target.checked)}
            /> Require human approval before merging
          </label>
          <p className="t2">
            When checked, a run stops once every automated gate passes (implementation, GitHub checks)
            and waits for an admin to approve or request changes on the workflow page, instead of merging automatically.
          </p>
        </fieldset>
        <div className="btnrow">
          <button type="submit" className="btn primary" disabled={saving}>{saving ? "Saving…" : submitLabel}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  );
}


function newPipelineStep(position: number): PipelineStep {
  return { command: "", timeoutSeconds: 300, position, required: true };
}

function movePipelineStep(steps: PipelineStep[], index: number, offset: number): PipelineStep[] {
  const target = index + offset;
  if (target < 0 || target >= steps.length) return steps;
  const result = [...steps];
  [result[index], result[target]] = [result[target], result[index]];
  return result;
}

const CREDENTIAL_LABELS: Record<GitCredentialKind, string> = {
  github_token: "GitHub token",
  ssh_private_key: "SSH private key",
};

const CREDENTIAL_HINTS: Record<GitCredentialKind, string> = {
  github_token: "Reads issues and opens pull requests as this token. Needs `repo` scope for a private repository.",
  ssh_private_key: "Used for git over SSH. Paste the private key, including its BEGIN and END lines.",
};

/** An agent credential is named by the operator, so its label is its name. */
function credentialLabel(kind: CredentialKind): string {
  return isAgentCredentialKind(kind) ? agentCredentialName(kind) : CREDENTIAL_LABELS[kind];
}

function credentialHint(kind: CredentialKind): string {
  if (!isAgentCredentialKind(kind)) return CREDENTIAL_HINTS[kind];
  return `Reaches the agent as $${agentCredentialName(kind)}. Delivered over the control stream, which requires the TLS overlay, and added to the runner's LOOP_RUNNER_ALLOWED_ENVIRONMENT.`;
}

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
  const [adding, setAdding] = useState(false);

  const load = useCallback(
    (signal: AbortSignal) => api.listProjectCredentials(projectId, signal),
    [api, projectId]
  );
  const { data, error, refresh } = usePolled(load, "Credentials are unavailable.", { poll: false });

  const configured = (kind: CredentialKind): ProjectCredential | undefined =>
    (data ?? []).find((entry) => entry.kind === kind);

  const clear = (kind: CredentialKind) => {
    api.clearProjectCredential(projectId, kind).then(
      () => { toast(`${credentialLabel(kind)} removed`); refresh(); },
      (reason: unknown) => toast(credentialFailure(reason))
    );
  };

  if (error) return <p className="t2">{error}</p>;

  const agentKinds = (data ?? []).map((entry) => entry.kind).filter(isAgentCredentialKind);
  const kinds: CredentialKind[] = [...(Object.keys(CREDENTIAL_LABELS) as GitCredentialKind[]), ...agentKinds];

  const row = (kind: CredentialKind) => {
    const entry = configured(kind);
    const agent = isAgentCredentialKind(kind);
    return (
      <div className="row-line" key={kind}>
        <span>
          {credentialLabel(kind)}
          <span className="t2">
            {entry
              ? ` · set ${ageAgo(entry.updatedAt, "recently")}${entry.filePath ? ` · file ~/${entry.filePath}` : ""}`
              : " · not set, uses the shared token"}
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
                  title: `Remove ${credentialLabel(kind)}`,
                  body: agent
                    ? "This project falls back to whatever the runner has configured under that name. Work already running is unaffected."
                    : "This project falls back to the deployment-wide credential. Work already running is unaffected.",
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
  };

  return (
    <div className="cred-list">
      {kinds.map(row)}
      {isAdmin && (
        <div className="btnrow">
          <button type="button" className="btn sm" onClick={() => setAdding(true)}>
            Add provider credential
          </button>
        </div>
      )}
      {editing && (
        <CredentialForm
          api={api}
          projectId={projectId}
          kind={editing}
          filePath={configured(editing)?.filePath ?? ""}
          onClose={() => setEditing(null)}
          onSaved={() => { toast(`${credentialLabel(editing)} saved`); refresh(); }}
        />
      )}
      {adding && (
        <AgentCredentialForm
          api={api}
          projectId={projectId}
          onClose={() => setAdding(false)}
          onSaved={(name) => { toast(`${name} saved`); refresh(); }}
        />
      )}
      {dialog}
    </div>
  );
}

/**
 * Adds a provider credential the agent needs: an OpenRouter key, a direct
 * provider key, or a subscription credentials document.
 *
 * The name is the environment variable the harness reads, because that is the
 * only thing the runner can deliver it under. A path turns it into a file
 * written below the execution's home directory — which is what a subscription
 * harness wants, and what an API key never does.
 */
function AgentCredentialForm({ api, projectId, onClose, onSaved }: {
  api: ApiClient;
  projectId: string;
  onClose: () => void;
  onSaved: (name: string) => void;
}) {
  const [name, setName] = useState("");
  const [filePath, setFilePath] = useState("");
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const variable = name.trim().toUpperCase();
    if (!AGENT_CREDENTIAL_NAME.test(variable)) {
      setError("Use the environment variable name the agent reads, e.g. OPENROUTER_API_KEY.");
      return;
    }
    if (!value.trim()) { setError("Paste the credential, or cancel."); return; }
    setError("");
    setSaving(true);
    api.setProjectCredential(projectId, `${AGENT_CREDENTIAL_PREFIX}${variable}`, value, filePath.trim()).then(
      () => { onSaved(variable); onClose(); },
      (reason: unknown) => setError(credentialFailure(reason))
    ).finally(() => setSaving(false));
  };

  return (
    <Modal title="Provider credential" onClose={onClose}>
      <h2>Provider credential</h2>
      <p>
        Reaches the agent under this name. The runner must also list it in
        LOOP_RUNNER_ALLOWED_ENVIRONMENT, and the control stream must be running
        the TLS overlay — the orchestrator will not send a credential in the clear.
      </p>
      <form onSubmit={submit}>
        {error && <ErrorBlock title={error} />}
        <div className="form-grid">
          <label>
            Variable
            <input
              value={name}
              placeholder="OPENROUTER_API_KEY"
              aria-label="Variable"
              onChange={(event) => setName(event.target.value)}
            />
          </label>
          <label>
            File path (optional)
            <input
              value={filePath}
              placeholder=".local/share/opencode/auth.json"
              aria-label="File path"
              onChange={(event) => setFilePath(event.target.value)}
            />
          </label>
          <label className="wide">
            Value
            <textarea
              value={value}
              rows={4}
              aria-label="Value"
              onChange={(event) => setValue(event.target.value)}
            />
          </label>
        </div>
        <p className="t2">
          Leave the path empty for an API key. Set it for a subscription harness
          that reads credentials from a file: it is written below the agent's
          home directory, and the variable carries the path.
        </p>
        <div className="btnrow">
          <button type="submit" className="btn primary" disabled={saving}>{saving ? "Saving…" : "Save"}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
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

function CredentialForm({ api, projectId, kind, filePath = "", onClose, onSaved }: {
  api: ApiClient;
  projectId: string;
  kind: CredentialKind;
  filePath?: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  // An SSH key and a subscription credentials document are both multi-line
  // pastes; a token and an API key are one line.
  const multiline = kind === "ssh_private_key" || filePath !== "";

  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!value.trim()) { setError("Paste the credential, or cancel."); return; }
    setError("");
    setSaving(true);
    // Replacing a credential keeps its delivery: an operator changing a
    // subscription token must not silently turn a file into a variable the
    // harness will never read.
    api.setProjectCredential(projectId, kind, value, filePath).then(
      () => { onSaved(); onClose(); },
      (reason: unknown) => setError(credentialFailure(reason))
    ).finally(() => setSaving(false));
  };

  return (
    <Modal title={credentialLabel(kind)} onClose={onClose}>
      <h2>{credentialLabel(kind)}</h2>
      <p>{credentialHint(kind)}</p>
      <form onSubmit={submit}>
        {error && <ErrorBlock title={error} />}
        <div className="form-grid">
          <label className="wide">
            Value
            {multiline ? (
              <textarea
                value={value}
                rows={6}
                aria-label={credentialLabel(kind)}
                onChange={(event) => setValue(event.target.value)}
              />
            ) : (
              <input
                type="password"
                value={value}
                autoComplete="off"
                aria-label={credentialLabel(kind)}
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
