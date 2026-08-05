// Workflow detail (specification.md §5.4): the thread, the decision that gates
// the merge, what the agent did, and the controls to stop it.
import { useCallback, useMemo, useState } from "react";
import { Link, useParams } from "react-router";
import type { ApiClient, Workflow, WorkflowEvent } from "./api";
import { ApiError } from "./api";
import { useConsoleData } from "./console-data-context";
import { absolute, clock } from "./format";
import { usePolled } from "./poll";
import { attemptRows, deriveGates, describeEvent, executionError, isTerminal, logText, statusMeta, PHASE_LABEL } from "./status";
import { useIsAdmin } from "./auth-context";
import {
  Age, Banner, Card, CardHeader, Empty, ErrorBlock, GateRow, KV, KVRow, Meter, Skeleton,
  StatusPill,
} from "./ui";
import { useConfirm } from "./ui/use-confirm";
import { useToast } from "./ui/toast-context";
import { AnsiLog } from "./ui/ansi";
import { PhaseThread, ThreadLegend } from "./ui/thread";

const EVENT_PAGE = 50;

export function WorkflowDetailPage({ api }: { api: ApiClient }) {
  const { workflowId } = useParams();
  const { projectName } = useConsoleData();
  const isAdmin = useIsAdmin();
  const toast = useToast();
  const { confirm, dialog } = useConfirm();
  const [busy, setBusy] = useState(false);

  const loadWorkflow = useCallback(
    (signal: AbortSignal) => api.getWorkflow(workflowId!, signal),
    [api, workflowId]
  );
  const workflow = usePolled(loadWorkflow, "This workflow could not be loaded.");

  if (!workflowId) return <ErrorBlock title="No workflow was named in the address." />;
  if (workflow.error && !workflow.data) {
    return <ErrorBlock title="This workflow could not be loaded." detail={workflow.error} onRetry={workflow.refresh} />;
  }
  if (!workflow.data) return <Skeleton cards={2} />;

  const run = workflow.data;
  const meta = statusMeta(run.status);
  const project = projectName(run.projectId);

  /** Runs a control action, then re-reads the run so the view shows the result. */
  const act = (label: string, call: () => Promise<unknown>, success: string) => {
    setBusy(true);
    call().then(
      () => {
        toast(success);
        workflow.refresh();
      },
      (reason: unknown) => {
        if (reason instanceof ApiError && reason.isForbidden) toast("You need the admin role for that");
        else toast(`${label} failed: ${reason instanceof Error ? reason.message : String(reason)}`);
        // A 409 means somebody else already decided; the refetch shows who won.
        workflow.refresh();
      }
    ).finally(() => setBusy(false));
  };

  return (
    <div>
      <div className="view-head">
        <span className="crumb"><Link to="/workflows">Workflows</Link> /</span>
        <h1>{project} {run.issueExternalId}</h1>
        <StatusPill status={run.status} />
      </div>
      <p className="view-sub">{run.issueTitle}</p>

      {run.status === "blocked" && (
        <Banner
          lead="Blocked."
          actions={isAdmin ? (
            <button
              type="button"
              className="btn sm"
              disabled={busy}
              onClick={() => act("Retry", () => api.retryWorkflow(run.id), "Issue reopened — the scheduler will start a fresh run")}
            >
              Retry with fresh context
            </button>
          ) : undefined}
        >
          {run.blockingReason || "No reason was recorded."}
        </Banner>
      )}

      {run.status === "failed" && (
        <Banner
          lead="Failed."
          actions={isAdmin ? (
            <button
              type="button"
              className="btn sm"
              disabled={busy}
              onClick={() => act("Retry", () => api.retryWorkflow(run.id), "Issue reopened — the scheduler will start a fresh run")}
            >
              Retry workflow
            </button>
          ) : undefined}
        >
          {run.blockingReason || "The run ended without delivering."}
        </Banner>
      )}

      {run.status === "cancelled" && (
        <Banner
          lead="Cancelled."
          actions={isAdmin ? (
            <button
              type="button"
              className="btn sm"
              disabled={busy}
              onClick={() => act("Retry", () => api.retryWorkflow(run.id), "Issue reopened — the scheduler will start a fresh run")}
            >
              Retry workflow
            </button>
          ) : undefined}
        >
          {run.blockingReason || "The run was cancelled."}
        </Banner>
      )}

      <Card className="thread-card gap-b">
        <CardHeader
          title="The thread"
          hint={<>phase {meta.phase ? PHASE_LABEL[meta.phase] : "terminal"} · updated <Age timestamp={run.updatedAt} /></>}
        />
        <div className="card-b">
          <PhaseThread workflow={run} />
          <ThreadLegend />
        </div>
      </Card>

      {run.status === "waiting_human" && (
        <DecisionPanel api={api} workflow={run} busy={busy} onAct={act} />
      )}

      <div className="detail-grid">
        <div className="stack">
          <History api={api} workflowId={run.id} />
        </div>

        <div className="stack">
          <Card>
            <CardHeader title="Gates" hint="derived from attempts" />
            <div className="card-b tight">
              {deriveGates(run).map((gate) => <GateRow key={gate.label} {...gate} />)}
            </div>
          </Card>

          <Card>
            <CardHeader title="Attempt budgets" hint="hard caps" />
            <div className="card-b tight">
              {attemptRows(run).map((row) => (
                <div className="row-line" key={row.label}>
                  <span>{row.emphasis ? <b>{row.label}</b> : row.label}</span>
                  <Meter used={row.used} budget={row.budget} label={row.label} />
                </div>
              ))}
            </div>
          </Card>

          <Card>
            <CardHeader title="Details" />
            <div className="card-b">
              <KV>
                <KVRow label="Workflow"><span className="num">{run.id}</span></KVRow>
                <KVRow label="Project">{project}</KVRow>
                <KVRow label="Branch"><span className="num">{run.branchName || "not created"}</span></KVRow>
                <KVRow label="PR">
                  {run.pullRequestUrl
                    ? <a className="num" href={run.pullRequestUrl} target="_blank" rel="noreferrer">#{run.pullRequestExternalId || "PR"}</a>
                    : <span className="t2">not yet</span>}
                </KVRow>
                <KVRow label="Started"><span title={absolute(run.createdAt)}><Age timestamp={run.createdAt} /></span></KVRow>
              </KV>
            </div>
          </Card>

          {isAdmin && !isTerminal(run.status) && run.status !== "waiting_human" && (
            <Controls api={api} workflow={run} busy={busy} confirm={confirm} onAct={act} />
          )}
        </div>
      </div>
      {dialog}
    </div>
  );
}

type ActFn = (label: string, call: () => Promise<unknown>, success: string) => void;

// --- Decision -------------------------------------------------------------

function DecisionPanel({ api, workflow, busy, onAct }: {
  api: ApiClient;
  workflow: Workflow;
  busy: boolean;
  onAct: ActFn;
}) {
  const isAdmin = useIsAdmin();
  const [comment, setComment] = useState("");

  if (!isAdmin) {
    return (
      <div className="decision gap-b">
        <h3>Waiting on a decision</h3>
        <p>This run is held at the human gate. An admin has to approve the merge or ask for changes.</p>
      </div>
    );
  }

  const decide = (decision: "approved" | "changes_requested", label: string, success: string) =>
    onAct(label, () => api.submitWorkflowDecision(workflow.id, decision, comment), success);

  return (
    <div className="decision gap-b">
      <h3>Your decision gates the merge</h3>
      <p>
        Every automated gate passed
        {workflow.pullRequestUrl ? <> on pull request{" "}
          <a className="num" href={workflow.pullRequestUrl} target="_blank" rel="noreferrer">
            #{workflow.pullRequestExternalId || "?"}
          </a></> : null}
        . Approving merges it with the project's merge method and closes issue {workflow.issueExternalId}.
      </p>
      <textarea
        value={comment}
        placeholder="Optional comment — recorded in the audit log and posted to the PR"
        aria-label="Decision comment"
        onChange={(event) => setComment(event.target.value)}
      />
      <div className="btnrow">
        <button
          type="button"
          className="btn primary"
          disabled={busy}
          onClick={() => decide("approved", "Approval", "Approved — merging the pull request and closing the issue")}
        >
          Approve &amp; merge
        </button>
        <button
          type="button"
          className="btn"
          disabled={busy}
          onClick={() => decide("changes_requested", "Request", "Changes requested — the developer is re-engaged")}
        >
          Request changes
        </button>
      </div>
    </div>
  );
}

// --- Controls -------------------------------------------------------------

function Controls({ api, workflow, busy, confirm, onAct }: {
  api: ApiClient;
  workflow: Workflow;
  busy: boolean;
  confirm: ReturnType<typeof useConfirm>["confirm"];
  onAct: ActFn;
}) {
  const [reason, setReason] = useState("");

  return (
    <Card>
      <CardHeader title="Controls" hint="admin" />
      <div className="card-b">
        <div className="reason-field">
          <textarea
            value={reason}
            aria-label="Reason"
            placeholder="Reason — required to block, recorded either way"
            onChange={(event) => setReason(event.target.value)}
          />
        </div>
        <div className="btnrow">
          <button
            type="button"
            className="btn"
            disabled={busy}
            onClick={() => confirm({
              title: "Cancel workflow",
              body: "The runner stops work, the project lock is released, and the issue goes back to agent:ready.",
              confirmLabel: "Cancel workflow",
              danger: true,
              onConfirm: () => onAct("Cancel", () => api.cancelWorkflow(workflow.id, reason), "Workflow cancelled — the issue is schedulable again"),
            })}
          >
            Cancel workflow
          </button>
          <button
            type="button"
            className="btn"
            disabled={busy || !reason.trim()}
            title={reason.trim() ? undefined : "A blocking reason is required"}
            onClick={() => confirm({
              title: "Block & hold issue",
              body: "The run is marked blocked and the issue is labelled agent:blocked, so the scheduler leaves it alone.",
              confirmLabel: "Block workflow",
              danger: true,
              onConfirm: () => onAct("Block", () => api.blockWorkflow(workflow.id, reason), "Workflow blocked — the issue is held"),
            })}
          >
            Block &amp; hold issue
          </button>
        </div>
      </div>
    </Card>
  );
}

// --- Events and log -------------------------------------------------------

/**
 * The timeline and the agent's log are two readings of the same rows, so they
 * share one poll rather than each asking for the page separately.
 */
function History({ api, workflowId }: { api: ApiClient; workflowId: string }) {
  const [older, setOlder] = useState<WorkflowEvent[]>([]);
  // undefined: nothing paged yet, follow the live page's cursor.
  // null: the history is exhausted, and asking again would replay the last page.
  const [pagedCursor, setPagedCursor] = useState<string | null | undefined>(undefined);
  const [loadingMore, setLoadingMore] = useState(false);
  const [moreError, setMoreError] = useState<string | null>(null);

  const load = useCallback(
    (signal: AbortSignal) => api.listWorkflowEvents(workflowId, { limit: EVENT_PAGE }, signal),
    [api, workflowId]
  );
  // Polled at the same cadence as the rest of the console, so a run in flight
  // grows its timeline without a reload (specification.md §4.5).
  const { data, error, refresh } = usePolled(load, "Events could not be loaded.");

  const events = useMemo(() => {
    const seen = new Set<string>();
    return [...(data?.events ?? []), ...older]
      .filter((event) => (seen.has(event.id) ? false : seen.add(event.id)))
      .sort((a, b) => b.createdAt.localeCompare(a.createdAt));
  }, [data, older]);

  const nextCursor = pagedCursor === undefined ? data?.nextCursor : pagedCursor ?? undefined;

  const loadMore = () => {
    if (!nextCursor) return;
    setLoadingMore(true);
    setMoreError(null);
    api.listWorkflowEvents(workflowId, { cursor: nextCursor, limit: EVENT_PAGE }).then(
      (page) => {
        setOlder((current) => [...current, ...page.events]);
        setPagedCursor(page.nextCursor ?? null);
      },
      (reason: unknown) => setMoreError(reason instanceof Error ? reason.message : "Could not load older events.")
    ).finally(() => setLoadingMore(false));
  };

  const lines = useMemo(
    () => events
      .filter((event) => event.type === "log")
      .map((event) => {
        const text = logText(event.payload);
        return text ? `${clock(event.createdAt)}  ${text}` : null;
      })
      .filter((line): line is string => line !== null)
      .reverse(),
    [events]
  );
  const errors = useMemo(
    () => [...new Set(events.map(executionError).filter((error): error is string => error !== null))],
    [events]
  );

  return (
    <>
      <Card>
        <CardHeader title="Events" hint="from workflow_events" />
        {error && !data && <div className="card-b"><ErrorBlock title="Events could not be loaded." detail={error} onRetry={refresh} /></div>}
        <div>
          {events.length === 0 && !error
            ? <Empty>No events have been recorded for this run.</Empty>
            : events.map((event) => {
              const described = describeEvent(event);
              return (
                <div className={described.warn ? "evt warn" : "evt"} key={event.id}>
                  <span className="tm" title={absolute(event.createdAt)}>{clock(event.createdAt)}</span>
                  <span className="ph">{described.phase}</span>
                  <span className="tx">{described.text}</span>
                </div>
              );
            })}
        </div>
        {(nextCursor || moreError) && (
          <div className="card-b">
            {moreError && <ErrorBlock title={moreError} />}
            {nextCursor && (
              <button type="button" className="btn sm" disabled={loadingMore} onClick={loadMore}>
                {loadingMore ? "Loading…" : "Load older events"}
              </button>
            )}
          </div>
        )}
      </Card>

      {errors.length > 0 && (
        <Card>
          <CardHeader title="Execution errors" />
          <div className="card-b">
            {errors.map((error) => <ErrorBlock key={error} title={error} />)}
          </div>
        </Card>
      )}

      {/* The runner already redacts and size-caps what it streams; specification
          task A4 adds a tail endpoint, at which point this stops reassembling the
          text from `log` events. */}
      <Card>
        <CardHeader title="Agent log" hint="redacted by the runner" />
        <div className="card-b">
          {error && !data && <ErrorBlock title="The agent log could not be loaded." detail={error} />}
          {!(error && !data) && (lines.length === 0
            ? <Empty>The agent has not written any log output.</Empty>
            : <AnsiLog className="logline" text={lines.join("\n")} />)}
        </div>
      </Card>
    </>
  );
}
