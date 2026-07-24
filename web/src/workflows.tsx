import { useEffect, useState } from "react";
import type { ApiClient, Workflow } from "./api";

export function WorkflowsPage({ api }: { api: ApiClient }) {
  const [workflows, setWorkflows] = useState<Workflow[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    const ctrl = new AbortController();
    api.listWorkflows(ctrl.signal).then(setWorkflows).catch(() => undefined).finally(() => setLoading(false));
    return () => ctrl.abort();
  }, [api]);

  if (loading) return <p>Loading workflows...</p>;

  return (
    <div>
      <h2>Workflows</h2>
      {workflows.length === 0 ? <p>No active workflows</p> : (
        <table>
          <thead><tr><th>ID</th><th>Project</th><th>Status</th><th>Phase</th></tr></thead>
          <tbody>
            {workflows.map((w) => (
              <tr key={w.id}>
                <td className="mono">{w.id.slice(0, 12)}</td>
                <td>{w.projectId}</td>
                <td>{w.status}</td>
                <td>{w.phase}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
