import type { Runner, Workflow } from "./api";

export type DashboardEvent = {
  type: "workflow" | "runner";
  workflow?: Workflow;
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
