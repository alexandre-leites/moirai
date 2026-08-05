// Queue (specification.md §5.2): one global queue across every enabled project,
// and — the column that matters — why each issue has not started.
import { useCallback } from "react";
import type { ApiClient, IssueSyncStatus } from "./api";
import { ApiError } from "./api";
import { useConsoleData } from "./console-data-context";
import { ageAgo, holdReason } from "./format";
import { usePolled } from "./poll";
import { useIsAdmin } from "./auth-context";
import { Age, Card, CardHeader, Empty, ErrorBlock, Pill, Skeleton, TableWrap } from "./ui";
import { useToast } from "./ui/toast-context";

/** Priority bands from the mockup: 6+ demands attention, 3+ is elevated. */
function PriorityPill({ priority }: { priority: number }) {
  const variant = priority >= 6 ? "bad" : priority >= 3 ? "warn" : "idle";
  return <span className={`pill p-${variant}`}>P{priority}</span>;
}

export function QueuePage({ api }: { api: ApiClient }) {
  const { data, error, loading, refresh } = useConsoleData();
  const load = useCallback((signal: AbortSignal) => api.issueSyncStatus(signal), [api]);
  const sync = usePolled(load, "Issue-sync status is unavailable.");

  const entries = data?.queue ?? [];

  return (
    <div>
      <div className="view-head"><h1>Queue</h1></div>
      <p className="view-sub">
        One global queue across all enabled projects. Highest <span className="num">agent-priority</span> wins;
        one workflow per project at a time.
      </p>

      {error && <ErrorBlock title={data ? "Showing the last good snapshot — the refresh failed." : "The queue could not be loaded."} detail={error} onRetry={refresh} />}
      {loading && !data && <Skeleton cards={1} />}

      {data && (
        <Card>
          <CardHeader title="Eligible issues" hint={`${entries.length} waiting`} />
          <TableWrap>
            <table>
              <thead>
                <tr><th>#</th><th>Priority</th><th>Issue</th><th>Why it hasn&apos;t started</th></tr>
              </thead>
              <tbody>
                {entries.length === 0 ? (
                  <tr><td colSpan={4}><Empty>The queue is empty. Every eligible issue has been picked up.</Empty></td></tr>
                ) : entries.map((entry, index) => (
                  <tr key={`${entry.projectId}:${entry.externalId}`}>
                    <td className="num t2">{index + 1}</td>
                    <td><PriorityPill priority={entry.priority} /></td>
                    <td>
                      <b>{entry.projectName} {entry.externalId}</b>
                      <div className="t2">{entry.title}</div>
                    </td>
                    <td className="t2">{holdReason(entry.blockedReason)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </TableWrap>
        </Card>
      )}

      <IssueSync api={api} entries={sync.data} error={sync.error} onRetry={sync.refresh} />
    </div>
  );
}

function IssueSync({ api, entries, error, onRetry }: {
  api: ApiClient;
  entries: IssueSyncStatus[] | null;
  error: string | null;
  onRetry: () => void;
}) {
  const isAdmin = useIsAdmin();
  const toast = useToast();

  const syncNow = () => {
    api.syncNow().then(
      (results) => {
        const failures = results.filter((result) => result.error);
        toast(failures.length === 0
          ? `Synced ${results.length === 1 ? "1 project" : `${results.length} projects`}`
          : `${failures.length} project${failures.length === 1 ? "" : "s"} failed to sync`);
        onRetry();
      },
      (reason: unknown) => {
        toast(reason instanceof ApiError && reason.isForbidden
          ? "You need the admin role for that"
          : `Sync failed: ${reason instanceof Error ? reason.message : String(reason)}`);
      }
    );
  };

  return (
    <Card className="section-gap">
      <CardHeader
        title="Issue sync"
        hint="polls the code host per enabled project"
        action={isAdmin ? <button type="button" className="btn sm" onClick={syncNow}>Sync now</button> : undefined}
      />
      {error && <div className="card-b"><ErrorBlock title="Issue-sync status is unavailable." detail={error} onRetry={onRetry} /></div>}
      {!error && (
        <TableWrap>
          <table>
            <thead>
              <tr><th>Project</th><th>Last sync</th><th>Eligible</th><th>Failures</th><th>Status</th></tr>
            </thead>
            <tbody>
              {(entries ?? []).length === 0 ? (
                <tr><td colSpan={5}><Empty>No project is being synchronized.</Empty></td></tr>
              ) : entries!.map((entry) => (
                <tr key={entry.projectId}>
                  <td><b>{entry.projectName}</b></td>
                  <td className="num"><Age timestamp={entry.lastSyncedAt} fallback="never" /></td>
                  <td className="num">{entry.eligibleCount}/{entry.issueCount}</td>
                  <td className="num">{entry.consecutiveFailures}</td>
                  <td>
                    {!entry.enabled ? <Pill variant="idle">Project paused</Pill>
                      : entry.backingOff ? (
                        <>
                          <Pill variant="warn">Backing off</Pill>{" "}
                          <span className="t2">
                            {entry.lastError || "the last pass failed"}
                            {entry.nextRetryAt && ` · retry in ${ageAgo(entry.nextRetryAt, "a moment").replace(" ago", "")}`}
                          </span>
                        </>
                      ) : <Pill variant="ok">Healthy</Pill>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableWrap>
      )}
    </Card>
  );
}
