// Filter/search predicates for the workflow list, split out of workflows.tsx
// so that file exports only components (react-refresh/only-export-components:
// these are plain functions, not components).
import type { Workflow } from "./api";
import { NEEDS_ATTENTION_STATUSES, isTerminal } from "./status";

export const FILTERS = [
  ["active", "Active"],
  ["needs_you", "Needs you"],
  ["terminal", "Terminal"],
  ["all", "All"],
] as const;

export type Filter = (typeof FILTERS)[number][0];

export const isFilter = (value: string | null): value is Filter =>
  FILTERS.some(([key]) => key === value);

export function matchesFilter(workflow: Workflow, filter: Filter): boolean {
  switch (filter) {
    case "active": return !isTerminal(workflow.status);
    case "needs_you": return NEEDS_ATTENTION_STATUSES.has(workflow.status);
    case "terminal": return isTerminal(workflow.status);
    case "all": return true;
  }
}

/** Search covers what an operator has in hand: an issue, a branch, or a PR. */
export function matchesQuery(workflow: Workflow, projectName: string, query: string): boolean {
  if (!query) return true;
  const needle = query.toLowerCase();
  return [
    projectName,
    workflow.issueExternalId,
    workflow.issueTitle,
    workflow.branchName,
    workflow.pullRequestExternalId,
  ].some((field) => field?.toLowerCase().includes(needle));
}
