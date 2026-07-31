import { useEffect, useState } from "react";
import type { ApiClient, QueueEntry } from "./api";

const BLOCKED_REASONS: Record<string, string> = {
  project_disabled: "Project disabled",
  project_locked: "Project has an active workflow",
  project_circuit_open: "Project circuit breaker open",
  provider_circuit_open: "Provider circuit breaker open",
  no_matching_runner: "No matching runner available",
};

export function QueuePage({ api }: { api: ApiClient }) {
  const [entries, setEntries] = useState<QueueEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ctrl = new AbortController();
    api
      .listQueue(ctrl.signal)
      .then(setEntries)
      .catch((err: unknown) => {
        if (!ctrl.signal.aborted) setError(err instanceof Error ? err.message : "Could not load the queue.");
      })
      .finally(() => {
        if (!ctrl.signal.aborted) setLoading(false);
      });
    return () => ctrl.abort();
  }, [api]);

  if (loading) return <p>Loading queue...</p>;
  if (error) return <p className="error" role="alert">Could not load the queue: {error}</p>;

  return (
    <div>
      <div className="view-head"><h1>Global queue</h1><span className="crumb">Scheduler</span></div>
      <p className="view-sub">Eligible issues, ranked by priority, and why anything is not being scheduled.</p>
      {entries.length === 0 ? <p className="empty-state">The queue is empty. No eligible issues are waiting.</p> : (
        <table>
          <thead><tr><th>Priority</th><th>Project</th><th>Issue</th><th>Blocked reason</th></tr></thead>
          <tbody>
            {entries.map((entry) => (
              <tr key={`${entry.projectId}:${entry.externalId}`}>
                <td className="mono">{entry.priority}</td>
                <td>{entry.projectName}</td>
                <td><span className="mono">{entry.externalId}</span> {entry.title}</td>
                <td>{BLOCKED_REASONS[entry.blockedReason] ?? (entry.blockedReason || "None")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
