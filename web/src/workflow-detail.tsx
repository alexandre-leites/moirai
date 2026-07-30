import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import type { ApiClient, WorkflowDetail, WorkflowEvent } from "./api";

function logText(payload: unknown): string | null {
  if (!payload || typeof payload !== "object") return typeof payload === "string" ? payload : null;
  const record = payload as Record<string, unknown>;
  const nested = record.payload;
  if (nested && typeof nested === "object") {
    const value = logText(nested);
    if (value) return value;
  }
  for (const key of ["message", "log", "text"]) {
    if (typeof record[key] === "string") return record[key];
  }
  return null;
}

function timestamp(value: string): string {
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
}

function newestFirst(events: WorkflowEvent[]): WorkflowEvent[] {
  return [...events].sort((left, right) => Date.parse(right.createdAt) - Date.parse(left.createdAt));
}

function TimelineEvent({ event }: { event: WorkflowEvent }) {
  const output = event.type === "log" ? logText(event.payload) : null;
  return (
    <li className="workflow-timeline-event">
      <div><strong>{event.type}</strong><time dateTime={event.createdAt}>{timestamp(event.createdAt)}</time></div>
      {output && <pre>{output}</pre>}
    </li>
  );
}

export function WorkflowDetailPage({ api }: { api: ApiClient }) {
  const { workflowId } = useParams();
  const [workflow, setWorkflow] = useState<WorkflowDetail | null>(null);
  const [events, setEvents] = useState<WorkflowEvent[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>();
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!workflowId) return;
    const ctrl = new AbortController();
    Promise.all([api.getWorkflow(workflowId, ctrl.signal), api.listWorkflowEvents(workflowId, undefined, ctrl.signal)])
      .then(([loadedWorkflow, page]) => {
        setWorkflow(loadedWorkflow);
        setEvents(newestFirst(page.events));
        setNextCursor(page.nextCursor);
      })
      .catch((err: unknown) => {
        if (!ctrl.signal.aborted) setError(err instanceof Error ? err.message : "Could not load workflow.");
      })
      .finally(() => {
        if (!ctrl.signal.aborted) setLoading(false);
      });
    return () => ctrl.abort();
  }, [api, workflowId]);

  async function loadMore() {
    if (!workflowId || !nextCursor) return;
    setLoadingMore(true);
    try {
      const page = await api.listWorkflowEvents(workflowId, nextCursor);
      setEvents((current) => newestFirst([...current, ...page.events]));
      setNextCursor(page.nextCursor);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not load more events.");
    } finally {
      setLoadingMore(false);
    }
  }

  if (!workflowId) return <p className="error" role="alert">Workflow ID is missing.</p>;
  if (loading) return <p>Loading workflow...</p>;
  if (error || !workflow) return <p className="error" role="alert">Could not load workflow: {error ?? "not found"}</p>;

  return (
    <div>
      <p><Link to="/workflows">← Workflows</Link></p>
      <h2>{workflow.issueExternalId}: {workflow.issueTitle}</h2>
      <section className="workflow-detail">
        <dl>
          <dt>Status</dt><dd>{workflow.status}</dd>
          <dt>Phase</dt><dd>{workflow.phase}</dd>
          <dt>Branch</dt><dd className="mono">{workflow.branchName || "Not created"}</dd>
          {workflow.blockingReason && <><dt>Blocking reason</dt><dd>{workflow.blockingReason}</dd></>}
          <dt>Pull request</dt>
          <dd>{workflow.pullRequestUrl ? <a href={workflow.pullRequestUrl} target="_blank" rel="noreferrer">{workflow.pullRequestUrl}</a> : "No pull request has been created."}</dd>
        </dl>
      </section>
      <section className="workflow-detail">
        <h3>Attempts</h3>
        <dl className="workflow-attempts">
          <dt>Planning</dt><dd>{workflow.planningAttempts}</dd>
          <dt>Implementation</dt><dd>{workflow.implementationAttempts}</dd>
          <dt>Pipeline repair</dt><dd>{workflow.pipelineRepairAttempts}</dd>
          <dt>CI repair</dt><dd>{workflow.ciRepairAttempts}</dd>
          <dt>AI review</dt><dd>{workflow.reviewCycles}</dd>
        </dl>
      </section>
      <section>
        <h3>Timeline</h3>
        {events.length === 0 ? <p className="empty-state">No runner events have been recorded.</p> : <ol className="workflow-timeline">{events.map((event) => <TimelineEvent key={event.id} event={event} />)}</ol>}
        {nextCursor && <button onClick={loadMore} disabled={loadingMore}>{loadingMore ? "Loading events..." : "Load older events"}</button>}
      </section>
    </div>
  );
}
