import { useCallback, useEffect, useState, type FormEvent } from "react";
import type { ApiClient, Project } from "./api";
import { useIsAdmin } from "./auth";
import { useControlPlaneEvents } from "./events";

export function ProjectsPage({ api }: { api: ApiClient }) {
  const [projects, setProjects] = useState<Project[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const isAdmin = useIsAdmin();
  const load = useCallback(async () => {
    try {
      setProjects(await api.listProjects());
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not load projects");
    } finally {
      setLoading(false);
    }
  }, [api]);

  useEffect(() => { void load(); }, [load]);
  useControlPlaneEvents(useCallback(() => { void load(); }, [load]));

  if (loading) return <p aria-live="polite">Loading projects...</p>;
  return <div>
    <h2>Projects</h2>
    {error && <p className="error" role="alert">{error}</p>}
    {isAdmin && <button onClick={() => setShowCreate(open => !open)}>{showCreate ? "Cancel" : "New project"}</button>}
    {isAdmin && showCreate && <CreateProjectForm api={api} onCreated={project => { setProjects(current => [...current, project]); setShowCreate(false); }} />}
    {projects.length === 0 ? <p>No projects registered</p> : <table><thead><tr><th>Name</th><th>Status</th>{isAdmin && <th>Actions</th>}</tr></thead><tbody>{projects.map(project => <tr key={project.id}><td>{project.name}</td><td>{project.enabled ? "Enabled" : "Disabled"}</td>{isAdmin && <td><ToggleProject api={api} project={project} onToggled={updated => setProjects(current => current.map(item => item.id === updated.id ? updated : item))} /></td>}</tr>)}</tbody></table>}
  </div>;
}

function CreateProjectForm({ api, onCreated }: { api: ApiClient; onCreated: (project: Project) => void }) {
  const [name, setName] = useState("");
  const [mode, setMode] = useState<"managed_clone" | "existing_path">("managed_clone");
  const [url, setURL] = useState("");
  const [localPath, setLocalPath] = useState("");
  const [defaultBranch, setDefaultBranch] = useState("main");
  const [labels, setLabels] = useState("");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const handleSubmit = async (event: FormEvent) => {
    event.preventDefault();
    const trimmedName = name.trim();
    const source = mode === "managed_clone" ? url.trim() : localPath.trim();
    if (!trimmedName) { setError("Project name is required"); return; }
    if (!source) { setError(mode === "managed_clone" ? "Repository URL is required" : "Local path is required"); return; }
    if (mode === "existing_path" && !source.startsWith("/")) { setError("Local path must be absolute"); return; }
    if (!defaultBranch.trim()) { setError("Default branch is required"); return; }
    setSaving(true); setError("");
    try {
      onCreated(await api.createProject({ name: trimmedName, repositoryMode: mode, repositoryUrl: mode === "managed_clone" ? source : undefined, localRepositoryPath: mode === "existing_path" ? source : undefined, defaultBranch: defaultBranch.trim(), requiredRunnerLabels: labels.split(",").map(label => label.trim()).filter(Boolean) }));
    } catch (err) { setError(err instanceof Error ? err.message : "Failed to create project"); } finally { setSaving(false); }
  };
  return <form onSubmit={handleSubmit} className="form"><h3>Create project</h3>{error && <p className="error" role="alert">{error}</p>}<label>Name <input value={name} onChange={event => setName(event.target.value)} disabled={saving} /></label><label>Repository mode <select value={mode} onChange={event => setMode(event.target.value as "managed_clone" | "existing_path")} disabled={saving}><option value="managed_clone">Managed clone (Git URL)</option><option value="existing_path">Existing local path</option></select></label>{mode === "managed_clone" ? <label>Repository URL <input value={url} onChange={event => setURL(event.target.value)} disabled={saving} /></label> : <label>Local path <input value={localPath} onChange={event => setLocalPath(event.target.value)} disabled={saving} /></label>}<label>Default branch <input value={defaultBranch} onChange={event => setDefaultBranch(event.target.value)} disabled={saving} /></label><label>Required runner labels (comma-separated) <input value={labels} onChange={event => setLabels(event.target.value)} disabled={saving} /></label><button type="submit" disabled={saving}>{saving ? "Creating..." : "Create"}</button></form>;
}

function ToggleProject({ api, project, onToggled }: { api: ApiClient; project: Project; onToggled: (project: Project) => void }) {
  const [error, setError] = useState<string | null>(null);
  const [toggling, setToggling] = useState(false);
  const handleToggle = async () => {
    setToggling(true); setError(null);
    try { onToggled(await api.setProjectEnabled(project.id, !project.enabled)); } catch (err) { setError(err instanceof Error ? err.message : "Could not update project"); } finally { setToggling(false); }
  };
  return <>{error && <p className="error" role="alert">{error}</p>}<button onClick={() => void handleToggle()} disabled={toggling}>{toggling ? "Updating..." : project.enabled ? "Disable" : "Enable"}</button></>;
}
