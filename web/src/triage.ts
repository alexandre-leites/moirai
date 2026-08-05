// "Needs you" list building, split out of overview.tsx so that file exports
// only components (react-refresh/only-export-components: triage is a plain
// function, not a component).
import type { Workflow } from "./api";
import { ageAgo } from "./format";

export type TriageItem = {
  kind: string;
  tone: "wait" | "crit" | "warn";
  workflow: Workflow;
  headline: string;
  detail: string;
  action: string;
};

/**
 * "Needs you", in the order specification.md §5.1 asks for: decisions first,
 * then blocked runs, then failures that left a pull request behind. Circuits sit
 * between the last two once task A7 exposes them.
 */
export function triage(workflows: Workflow[]): TriageItem[] {
  const items: TriageItem[] = [];

  for (const workflow of workflows.filter((w) => w.status === "waiting_human")) {
    items.push({
      kind: "decision",
      tone: "wait",
      workflow,
      headline: "is ready to merge",
      detail: `Every automated gate passed. Waiting ${ageAgo(workflow.updatedAt, "for a while").replace(" ago", "")} for your decision.`,
      action: "Review & decide",
    });
  }

  for (const workflow of workflows.filter((w) => w.status === "blocked")) {
    items.push({
      kind: "blocked",
      tone: "crit",
      workflow,
      headline: "is blocked",
      detail: workflow.blockingReason || "No reason was recorded.",
      action: "Inspect",
    });
  }

  for (const workflow of workflows.filter((w) => w.status === "failed" && Boolean(w.pullRequestUrl))) {
    items.push({
      kind: "failed",
      tone: "warn",
      workflow,
      headline: "failed with an open pull request",
      detail: workflow.blockingReason || "The run ended before the pull request was merged or closed.",
      action: "Inspect",
    });
  }

  return items;
}
