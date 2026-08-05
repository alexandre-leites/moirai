// The context, the hook that reads it, and the pure selectors derived from
// the snapshot — split out of console-data.tsx so that file can export only
// the ConsoleDataProvider component (react-refresh/only-export-components).
import { createContext, useContext } from "react";
import type { Project, QueueEntry, Runner, Workflow } from "./api";
import { describeHeartbeat } from "./runner-status";
import { NEEDS_ATTENTION_STATUSES, isTerminal } from "./status";
import type { Polled } from "./poll";

export type ConsoleSnapshot = {
  workflows: Workflow[];
  runners: Runner[];
  queue: QueueEntry[];
  projects: Project[];
};

export type ConsoleData = Polled<ConsoleSnapshot> & {
  /** Project id → display name, for the "<project> #<issue>" headline. */
  projectName: (id: string) => string;
};

export const ConsoleDataContext = createContext<ConsoleData | null>(null);

export function useConsoleData(): ConsoleData {
  const value = useContext(ConsoleDataContext);
  if (!value) throw new Error("useConsoleData must be used inside ConsoleDataProvider");
  return value;
}

// --- Derived views over the snapshot --------------------------------------

export const activeWorkflows = (workflows: Workflow[]): Workflow[] =>
  workflows.filter((workflow) => !isTerminal(workflow.status));

export const needsAttention = (workflows: Workflow[]): Workflow[] =>
  workflows.filter((workflow) => NEEDS_ATTENTION_STATUSES.has(workflow.status));

/**
 * Runners the orchestrator can currently reach. A stale heartbeat does not
 * count: `runners.status` only falls back to `offline` through lease expiry or
 * revocation, so a killed idle runner would otherwise read online forever
 * (see describeRunnerStatus in runner-status.ts).
 */
export const onlineRunners = (runners: Runner[], now: number = Date.now()): Runner[] =>
  runners.filter((runner) => runner.status === "online" && !describeHeartbeat(runner.lastSeenAt, now).stale);

/**
 * The run holding a project's lock, as far as the workflow list can tell.
 *
 * `app.project_locks` is the authority; specification task A11 exposes it on the
 * project payload. Until then the non-terminal run for that project is the same
 * row, because the scheduler admits one at a time.
 */
export const activeWorkflowFor = (workflows: Workflow[], projectId: string): Workflow | undefined =>
  activeWorkflows(workflows).find((workflow) => workflow.projectId === projectId);
