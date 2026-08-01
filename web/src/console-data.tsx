// The collections nearly every view needs, polled once for the whole console.
//
// Workflows, runners, the queue and projects are read by the sidebar counts, the
// overview, and at least one view each. Fetching them per view would mean the
// same four requests several times over on one screen; fetching them here in
// parallel keeps it to one round of four (specification.md §6, "Performance").
import { createContext, useCallback, useContext, useMemo, type ReactNode } from "react";
import type { ApiClient, Project, QueueEntry, Runner, Workflow } from "./api";
import { usePolled, type Polled } from "./poll";
import { describeHeartbeat } from "./runner-status";
import { NEEDS_ATTENTION_STATUSES, isTerminal } from "./status";

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

const ConsoleDataContext = createContext<ConsoleData | null>(null);

export function ConsoleDataProvider({ api, children }: { api: ApiClient; children: ReactNode }) {
  const load = useCallback(
    async (signal: AbortSignal): Promise<ConsoleSnapshot> => {
      const [workflows, runners, queue, projects] = await Promise.all([
        api.listWorkflows(signal),
        api.listRunners(signal),
        api.listQueue(signal),
        api.listProjects(signal),
      ]);
      return { workflows, runners, queue, projects };
    },
    [api]
  );

  const polled = usePolled(load, "The control plane could not be reached.");

  const value = useMemo<ConsoleData>(() => {
    const names = new Map((polled.data?.projects ?? []).map((project) => [project.id, project.name]));
    return { ...polled, projectName: (id: string) => names.get(id) ?? id };
  }, [polled]);

  return <ConsoleDataContext.Provider value={value}>{children}</ConsoleDataContext.Provider>;
}

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
