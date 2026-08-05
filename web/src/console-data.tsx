// The collections nearly every view needs, polled once for the whole console.
//
// Workflows, runners, the queue and projects are read by the sidebar counts, the
// overview, and at least one view each. Fetching them per view would mean the
// same four requests several times over on one screen; fetching them here in
// parallel keeps it to one round of four (specification.md §6, "Performance").
import { useCallback, useEffect, useMemo, type ReactNode } from "react";
import type { ApiClient } from "./api";
import { ConsoleDataContext, type ConsoleData, type ConsoleSnapshot } from "./console-data-context";
import { subscribeEvents } from "./events";
import { usePolled } from "./poll";

function replaceByID<T extends { id: string }>(items: T[], next: T): T[] {
  const index = items.findIndex((item) => item.id === next.id);
  if (index < 0) return [next, ...items];
  return items.map((item) => item.id === next.id ? next : item);
}

/**
 * Like replaceByID, but patches a matching row instead of swapping it out
 * wholesale — for events whose payload only carries a subset of the row's
 * fields (e.g. the SSE workflow event's `WorkflowLifecycle` shape). A row
 * with no existing match is inserted as-is, same as replaceByID: there is
 * nothing to wipe for a workflow the snapshot hasn't loaded yet, and the next
 * poll fills in the rest.
 */
function mergeByID<T extends { id: string }>(items: T[], patch: Partial<T> & { id: string }): T[] {
  const index = items.findIndex((item) => item.id === patch.id);
  if (index < 0) return [patch as T, ...items];
  return items.map((item) => item.id === patch.id ? { ...item, ...patch } : item);
}

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
  const { update } = polled;

  useEffect(() => subscribeEvents((event) => {
    update((snapshot) => {
      if (event.workflow) {
        return { ...snapshot, workflows: mergeByID(snapshot.workflows, event.workflow) };
      }
      if (event.runner) {
        return { ...snapshot, runners: replaceByID(snapshot.runners, event.runner) };
      }
      return snapshot;
    });
  }), [update]);

  const value = useMemo<ConsoleData>(() => {
    const names = new Map((polled.data?.projects ?? []).map((project) => [project.id, project.name]));
    return { ...polled, projectName: (id: string) => names.get(id) ?? id };
  }, [polled]);

  return <ConsoleDataContext.Provider value={value}>{children}</ConsoleDataContext.Provider>;
}
