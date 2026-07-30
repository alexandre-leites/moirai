import { useEffect, useState } from "react";
import type { ApiClient, Workflow } from "./api";

const AWAITING_APPROVAL_STATUS = "waiting_human";
const TERMINAL_STATUSES = new Set(["blocked", "failed", "cancelled"]);

export function WorkflowsPage({ api }: { api: ApiClient }) {
  const [workflows, setWorkflows] = useState<Workflow[]>([]);
  const [loading, setLoading] = useState(true);
  const [pendingId, setPendingId] = useState<string | null>(null);
  const [reasonByID, setReasonByID] = useState<Record<string, string>>({});
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ctrl = new AbortController();
    api.listWorkflows(ctrl.signal).then(setWorkflows).catch(() => undefined).finally(() => setLoading(false));
    return () => ctrl.abort();
  }, [api]);

  async function control(id: string, action: "retry" | "cancel" | "block") {
    const reason = reasonByID[id] ?? "";
    if (action === "block" && !reason.trim()) {
      setError("A blocking reason is required.");
      return;
    }
    setPendingId(id);
    setError(null);
    try {
      const updated = action === "retry"
        ? await api.retryWorkflow(id, reason)
        : action === "cancel"
          ? await api.cancelWorkflow(id, reason)
          : await api.blockWorkflow(id, reason);
      setWorkflows((current) => current.map((w) => (w.id === id ? updated : w)));
    } catch {
      setError(`Could not ${action} the workflow. Please try again.`);
    } finally {
      setPendingId(null);
    }
  }

  async function decide(id: string, decision: "approved" | "changes_requested") {
    setPendingId(id);
    setError(null);
    try {
      const updated = await api.submitWorkflowDecision(id, decision);
      setWorkflows((current) => current.map((w) => (w.id === id ? updated : w)));
    } catch {
      setError("Could not submit the decision. Please try again.");
    } finally {
      setPendingId(null);
    }
  }

  if (loading) return <p>Loading workflows...</p>;

  return (
    <div>
      <h2>Workflows</h2>
      {error && <p className="error">{error}</p>}
      {workflows.length === 0 ? <p>No active workflows</p> : (
        <table>
          <thead><tr><th>ID</th><th>Project</th><th>Status</th><th>Phase</th><th>Approval</th><th>Controls</th></tr></thead>
          <tbody>
            {workflows.map((w) => (
              <tr key={w.id}>
                <td className="mono">{w.id.slice(0, 12)}</td>
                <td>{w.projectId}</td>
                <td>{w.status}</td>
                <td>{w.phase}</td>
                <td>
                  {w.status === AWAITING_APPROVAL_STATUS ? (
                    <span className="workflow-decision-actions">
                      <button disabled={pendingId === w.id} onClick={() => decide(w.id, "approved")}>Approve</button>
                      <button disabled={pendingId === w.id} onClick={() => decide(w.id, "changes_requested")}>Request changes</button>
                    </span>
                  ) : null}
                </td>
                <td>
                  {TERMINAL_STATUSES.has(w.status) ? (
                    <button disabled={pendingId === w.id} onClick={() => control(w.id, "retry")}>Retry</button>
                  ) : w.status !== "completed" ? (
                    <span className="workflow-decision-actions">
                      <input
                        aria-label={`Reason for ${w.id}`}
                        value={reasonByID[w.id] ?? ""}
                        onChange={(event) => setReasonByID((current) => ({ ...current, [w.id]: event.target.value }))}
                      />
                      <button disabled={pendingId === w.id} onClick={() => control(w.id, "cancel")}>Cancel</button>
                      <button disabled={pendingId === w.id} onClick={() => control(w.id, "block")}>Block</button>
                    </span>
                  ) : null}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
