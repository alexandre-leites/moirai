// Projects (specification.md §5.6). Each project is one repository the Fates
// may work on — its issues, its labels, its delivery policy.
import { useState, type FormEvent } from "react";
import { Link } from "react-router";
import type { ApiClient, Project, ProjectConfiguration } from "./api";
import { ApiError } from "./api";
import { activeWorkflowFor, useConsoleData } from "./console-data";
import { useIsAdmin } from "./auth";
import {
  Card, Empty, ErrorBlock, KV, KVRow, Modal, Pill, Skeleton, useToast,
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
        </div>
        <div className="btnrow">
          <button type="submit" className="btn primary" disabled={saving}>{saving ? "Saving…" : submitLabel}</button>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
        </div>
      </form>
    </Modal>
  );
}
