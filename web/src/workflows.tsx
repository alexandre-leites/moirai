// Workflow list (specification.md §5.3). Filter and search live in the query
// string, so an operator can link a colleague straight at what they are looking at.
import { useEffect, useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { useConsoleData } from "./console-data-context";
import { ATTEMPT_BUDGETS } from "./status";
import {
  Age, Card, Empty, ErrorBlock, FilterChips, Meter, RowLink, Skeleton, StatusPill, TableWrap,
} from "./ui";
import { PhaseThread } from "./ui/thread";
import { FILTERS, isFilter, matchesFilter, matchesQuery, type Filter } from "./workflow-filters";

export function WorkflowsPage() {
  const navigate = useNavigate();
  const { data, error, loading, refresh, projectName } = useConsoleData();
  const [params, setParams] = useSearchParams();

  const filter: Filter = isFilter(params.get("filter")) ? (params.get("filter") as Filter) : "active";
  const query = params.get("q") ?? "";
  // The box stays responsive while the URL — and therefore the filtering — lags
  // behind by a beat, so typing never feels like it stutters.
  const [draft, setDraft] = useState(query);

  useEffect(() => { setDraft(query); }, [query]);

  useEffect(() => {
    if (draft === query) return;
    const timer = setTimeout(() => {
      setParams((current) => {
        const next = new URLSearchParams(current);
        if (draft) next.set("q", draft);
        else next.delete("q");
        return next;
      }, { replace: true });
    }, 300);
    return () => clearTimeout(timer);
  }, [draft, query, setParams]);

  const rows = useMemo(() => {
    const workflows = data?.workflows ?? [];
    return workflows.filter(
      (workflow) => matchesFilter(workflow, filter) && matchesQuery(workflow, projectName(workflow.projectId), query)
    );
  }, [data, filter, query, projectName]);

  const setFilter = (next: Filter) => {
    setParams((current) => {
      const params = new URLSearchParams(current);
      params.set("filter", next);
      return params;
    });
  };

  return (
    <div>
      <div className="view-head"><h1>Workflows</h1></div>
      <p className="view-sub">
        Every thread the orchestrator has spun — open one for phases, attempts, events, and controls.
      </p>

      <div className="filters" role="group" aria-label="Filter workflows">
        <FilterChips options={FILTERS} value={filter} onChange={setFilter} label="Workflow filter" />
        <span className="gap" />
        <input
          className="search"
          type="search"
          value={draft}
          placeholder="Search issue, branch, PR…"
          aria-label="Search workflows"
          onChange={(event) => setDraft(event.target.value)}
        />
      </div>

      {error && data && <ErrorBlock title="Showing the last good snapshot — the refresh failed." detail={error} onRetry={refresh} />}
      {error && !data && <ErrorBlock title="Workflows could not be loaded." detail={error} onRetry={refresh} />}
      {loading && !data && <Skeleton cards={1} />}

      {data && (
        <Card>
          <TableWrap>
            <table>
              <thead>
                <tr>
                  <th>Issue</th><th>Status</th><th>Thread</th><th>Attempts</th>
                  <th>Branch</th><th>PR</th><th>Updated</th>
                </tr>
              </thead>
              <tbody>
                {rows.length === 0 ? (
                  <tr><td colSpan={7}><Empty>No workflows match this filter.</Empty></td></tr>
                ) : rows.map((workflow) => (
                  <RowLink
                    key={workflow.id}
                    onOpen={() => navigate(`/workflows/${workflow.id}`)}
                    label={`Open workflow for ${projectName(workflow.projectId)} issue ${workflow.issueExternalId}`}
                  >
                    <td>
                      <b>{projectName(workflow.projectId)} {workflow.issueExternalId}</b>
                      <div className="t2 cell-title">{workflow.issueTitle}</div>
                    </td>
                    <td><StatusPill status={workflow.status} /></td>
                    <td><PhaseThread workflow={workflow} mini /></td>
                    <td>
                      <Meter used={workflow.totalAgentExecutions} budget={ATTEMPT_BUDGETS.totalExecutions} label="Total executions" />
                    </td>
                    <td className="num t2">{workflow.branchName || "—"}</td>
                    <td>
                      {workflow.pullRequestUrl ? (
                        <a
                          className="num"
                          href={workflow.pullRequestUrl}
                          target="_blank"
                          rel="noreferrer"
                          onClick={(event) => event.stopPropagation()}
                        >
                          #{workflow.pullRequestExternalId || "PR"}
                        </a>
                      ) : <span className="t2">—</span>}
                    </td>
                    <td className="t2 nowrap"><Age timestamp={workflow.updatedAt} /></td>
                  </RowLink>
                ))}
              </tbody>
            </table>
          </TableWrap>
        </Card>
      )}
    </div>
  );
}
