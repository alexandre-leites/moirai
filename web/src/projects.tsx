import { useEffect, useState, type FormEvent } from "react";
import type { ApiClient, Project } from "./api";
import { useIsAdmin } from "./auth";

export function ProjectsPage({ api }: { api: ApiClient }) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const isAdmin = useIsAdmin();

  useEffect(() => {
    const ctrl = new AbortController();
    api.listProjects(ctrl.signal).then(setProjects).catch(() => undefined).finally(() => setLoading(false));
    return () => ctrl.abort();
  }, [api]);

  if (loading) return <p>Loading projects...</p>;

  return (
    <div>
      <h2>Projects</h2>
      {isAdmin && (
        <button onClick={() => setShowCreate(!showCreate)}>
          {showCreate ? "Cancel" : "New project"}
        </button>
      )}
      {isAdmin && showCreate && <CreateProjectForm api={api} onCreated={(p) => { setProjects([...projects, p]); setShowCreate(false); }} />}
      {projects.length === 0 ? <p>No projects registered</p> : (
        <table>
          <thead><tr><th>Name</th><th>Status</th>{isAdmin && <th>Actions</th>}</tr></thead>
          <tbody>
            {projects.map((p) => (
              <tr key={p.id}>
                <td>{p.name}</td>
                <td>{p.enabled ? "Enabled" : "Disabled"}</td>
                {isAdmin && (
                  <td>
                    <ToggleProject api={api} project={p} onToggled={(updated) => {
                      setProjects(projects.map((x) => x.id === updated.id ? updated : x));
                    }} />
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function CreateProjectForm({ api, onCreated }: { api: ApiClient; onCreated: (p: Project) => void }) {
  const [name, setName] = useState("");
  const [mode, setMode] = useState<"managed_clone" | "existing_path">("managed_clone");
  const [url, setUrl] = useState("");
  const [localPath, setLocalPath] = useState("");
  const [defaultBranch, setDefaultBranch] = useState("main");
  const [labels, setLabels] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    if (!name.trim()) { setError("Project name is required"); return; }
    setSaving(true);
    try {
      const project = await api.createProject({
        name: name.trim(),
        repositoryMode: mode,
        repositoryUrl: mode === "managed_clone" ? url.trim() : undefined,
        localRepositoryPath: mode === "existing_path" ? localPath.trim() : undefined,
        defaultBranch: defaultBranch.trim() || "main",
        requiredRunnerLabels: labels.split(",").map((l) => l.trim()).filter(Boolean),
      });
      onCreated(project);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create project");
    } finally {
      setSaving(false);
    }
  };

  return (
    <form onSubmit={handleSubmit} className="form">
      <h3>Create project</h3>
      {error && <p className="error">{error}</p>}
      <label>Name <input value={name} onChange={(e) => setName(e.target.value)} disabled={saving} /></label>
      <label>Repository mode
        <select value={mode} onChange={(e) => setMode(e.target.value as "managed_clone" | "existing_path")}>
          <option value="managed_clone">Managed clone (Git URL)</option>
          <option value="existing_path">Existing local path</option>
        </select>
      </label>
      {mode === "managed_clone" && (
        <label>Repository URL <input value={url} onChange={(e) => setUrl(e.target.value)} placeholder="git@github.com:owner/repo.git" /></label>
      )}
      {mode === "existing_path" && (
        <label>Local path <input value={localPath} onChange={(e) => setLocalPath(e.target.value)} placeholder="/repositories/my-service" /></label>
      )}
      <label>Default branch <input value={defaultBranch} onChange={(e) => setDefaultBranch(e.target.value)} /></label>
      <label>Required runner labels (comma-separated) <input value={labels} onChange={(e) => setLabels(e.target.value)} placeholder="linux, docker" /></label>
      <button type="submit" disabled={saving}>{saving ? "Creating..." : "Create"}</button>
    </form>
  );
}

function ToggleProject({ api, project, onToggled }: { api: ApiClient; project: Project; onToggled: (p: Project) => void }) {
  const [toggling, setToggling] = useState(false);
  const handleToggle = async () => {
    setToggling(true);
    try {
      const updated = await api.setProjectEnabled(project.id, !project.enabled);
      onToggled(updated);
    } catch {
      // silently fail; the API client error is sufficient
    } finally {
      setToggling(false);
    }
  };
  return (
    <button onClick={handleToggle} disabled={toggling}>
      {toggling ? "..." : project.enabled ? "Disable" : "Enable"}
    </button>
  );
}
