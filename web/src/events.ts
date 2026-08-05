import type { Runner, WorkflowLifecycle } from "./api";

/**
 * `workflow` carries only the lifecycle fields (id/projectId/status/phase):
 * that's all `writeSSEEvent` in api/internal/http/handlers/events.go sends,
 * mirroring the control endpoints' `workflowPayload` shape rather than the
 * full row `workflowDetailPayload` builds. Typing it as the full `Workflow`
 * would let a consumer replace a complete row with this stub and wipe
 * `issueTitle`, `pullRequestUrl`, attempt counters, etc. — see console-data's
 * `mergeByID`, which patches the existing row instead.
 */
export type DashboardEvent = {
  type: "workflow" | "runner";
  workflow?: WorkflowLifecycle;
  runner?: Runner;
};

type EventSourceLike = Pick<EventSource, "close" | "addEventListener">;
type EventSourceFactory = (url: string) => EventSourceLike;

export function subscribeEvents(
  onEvent: (event: DashboardEvent) => void,
  create?: EventSourceFactory
): () => void {
  if (!create && typeof EventSource === "undefined") return () => undefined;
  const source = (create ?? ((url) => new EventSource(url)))("/api/v1/events");
  const receive = (event: Event) => {
    const data = event as MessageEvent<string>;
    try {
      onEvent(JSON.parse(data.data) as DashboardEvent);
    } catch {
      return;
    }
  };
  source.addEventListener("workflow", receive);
  source.addEventListener("runner", receive);
  return () => source.close();
}
