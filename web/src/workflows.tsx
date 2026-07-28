import { useCallback, useEffect, useState } from "react";
import type { ApiClient, Workflow } from "./api";
import { useControlPlaneEvents } from "./events";
import { Link } from "react-router-dom";

export function WorkflowsPage({ api }: { api: ApiClient }) {
  const [workflows, setWorkflows] = useState<Workflow[]>([]); const [loading, setLoading] = useState(true); const [pendingId, setPendingId] = useState<string | null>(null); const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => { try { setWorkflows(await api.listWorkflows()); setError(null); } catch (err) { setError(err instanceof Error ? err.message : "Could not load workflows"); } finally { setLoading(false); } }, [api]);
  useEffect(() => { void load(); }, [load]); useControlPlaneEvents(useCallback(() => { void load(); }, [load]));
  const action = async (id: string, actionName: "retry" | "cancel" | "block" | "approved" | "changes_requested") => { setPendingId(id); setError(null); try { const updated = actionName === "approved" || actionName === "changes_requested" ? await api.submitWorkflowDecision(id, actionName) : await api.workflowAction(id, actionName); setWorkflows(current => current.map(w => w.id === id ? updated : w)); } catch (err) { setError(err instanceof Error ? err.message : "Workflow action failed"); } finally { setPendingId(null); } };
  if (loading) return <p aria-live="polite">Loading workflows...</p>;
  return <div><h2>Workflows</h2>{error && <p className="error" role="alert">{error}</p>}{workflows.length === 0 ? <p>No active workflows</p> : <table><thead><tr><th>ID</th><th>Project</th><th>Status</th><th>Phase</th><th>Actions</th></tr></thead><tbody>{workflows.map(w => <tr key={w.id}><td className="mono"><Link to={`/workflows/${w.id}`}>{w.id.slice(0, 12)}</Link></td><td>{w.projectId}</td><td>{w.status}</td><td>{w.phase}</td><td className="workflow-decision-actions">{w.status === "waiting_human" && <><button disabled={pendingId === w.id} onClick={() => void action(w.id, "approved")}>Approve</button><button disabled={pendingId === w.id} onClick={() => void action(w.id, "changes_requested")}>Request changes</button></>}{["blocked", "failed", "cancelled"].includes(w.status) && <button disabled={pendingId === w.id} onClick={() => void action(w.id, "retry")}>Retry</button>}{!["completed", "cancelled"].includes(w.status) && <button disabled={pendingId === w.id} onClick={() => void action(w.id, "cancel")}>Cancel</button>}{!["completed", "blocked", "cancelled"].includes(w.status) && <button disabled={pendingId === w.id} onClick={() => void action(w.id, "block")}>Block</button>}</td></tr>)}</tbody></table>}</div>;
}

export function WorkflowDetailPage({ api, id }: { api: ApiClient; id: string }) {
  const [data, setData] = useState<{ workflow: Workflow; events: { id: string; type: string; severity: string; payload: string; createdAt: string }[] } | null>(null); const [error, setError] = useState<string | null>(null);
  const load = useCallback(async () => { try { setData(await api.getWorkflow(id)); setError(null); } catch (err) { setError(err instanceof Error ? err.message : "Could not load workflow"); } }, [api, id]);
  useEffect(() => { void load(); }, [load]); useControlPlaneEvents(useCallback(event => { if (event.resourceId === id) void load(); }, [id, load]));
  if (error) return <p className="error" role="alert">{error}</p>; if (!data) return <p aria-live="polite">Loading workflow...</p>; const w = data.workflow;
  return <div><h2>Workflow {w.id}</h2><dl><dt>Status</dt><dd>{w.status}</dd><dt>Phase</dt><dd>{w.phase}</dd><dt>Attempts</dt><dd>Planning {w.planningAttempts ?? 0}, implementation {w.implementationAttempts ?? 0}, pipeline repairs {w.pipelineRepairAttempts ?? 0}, reviews {w.reviewCycles ?? 0}, CI repairs {w.ciRepairAttempts ?? 0}</dd>{w.blockingReason && <><dt>Blocking reason</dt><dd>{w.blockingReason}</dd></>}</dl><h3>Event history</h3><ul className="event-list">{data.events.map(event => <li key={event.id}><strong>{event.type}</strong> {event.createdAt} <code>{event.payload}</code></li>)}</ul></div>;
}
