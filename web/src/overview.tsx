// Overview (specification.md §5.1). The organizing principle of the console:
// human decisions and stuck work come before counts and lists.
import { useCallback, useMemo } from "react";
import { Link } from "react-router";
import type { ApiClient, Health, IssueSyncStatus, SchedulerMetrics, Workflow, WorkflowEvent } from "./api";
import { activeWorkflows, onlineRunners, useConsoleData } from "./console-data";
import { ageAgo, plural, shortClock } from "./format";
import { usePolled } from "./poll";
import { CUT_STATUSES, describeEvent, isTerminal, statusMeta } from "./status";
import {
  AttentionItem, Card, CardHeader, Empty, ErrorBlock, FeedItem, HealthStrip, Probe,
  Skeleton, StatTile, StatusPill,
} from "./ui";
import { PhaseThread } from "./ui/thread";

/** How many active runs the event feed samples, and how deep into each. */
const FEED_WORKFLOWS = 5;
const FEED_EVENTS_PER_WORKFLOW = 5;
const FEED_LENGTH = 10;

type Vitals = {
  health: Health;
  metrics: SchedulerMetrics;
  sync: IssueSyncStatus[];
};

export function OverviewPage({ api }: { api: ApiClient }) {
  const { data, error, loading, refresh, projectName } = useConsoleData();

  const loadVitals = useCallback(
    async (signal: AbortSignal): Promise<Vitals> => {
      const [health, metrics, sync] = await Promise.all([
        api.health(signal),
        api.schedulerMetrics(signal),
        api.issueSyncStatus(signal),
      ]);
      return { health, metrics, sync };
    },
    [api]
  );
  const vitals = usePolled(loadVitals, "Orchestrator vitals are unavailable.");

  const active = useMemo(() => activeWorkflows(data?.workflows ?? []), [data]);
  const attention = useMemo(() => triage(data?.workflows ?? []), [data]);

  if (error && !data) {
    return (
      <div>
        <Head count={0} />
        <ErrorBlock title="The console could not load." detail={error} onRetry={refresh} />
      </div>
    );
  }
  if (loading && !data) {
    return (
      <div>
        <Head count={0} />
        <Skeleton cards={3} />
      </div>
    );
  }

  const workflows = data?.workflows ?? [];
  const runners = data?.runners ?? [];
  const queue = data?.queue ?? [];
  const online = onlineRunners(runners);
  const draining = runners.filter((runner) => runner.draining);
  const offline = runners.filter((runner) => runner.status !== "online");
  // Reported online but silent past its heartbeat budget. Worth naming apart
  // from offline: the orchestrator still believes these are available.
  const stale = runners.filter((runner) => runner.status === "online" && !online.includes(runner));
  const spinning = active.filter((workflow) => statusMeta(workflow.status).variant === "run");
  const waiting = workflows.filter((workflow) => workflow.status === "waiting_human");
  const delivered = workflows.filter((workflow) => workflow.status === "completed");
  const blocked = workflows.filter((workflow) => workflow.status === "blocked");
  const failed = workflows.filter((workflow) => workflow.status === "failed");
  const topPriority = queue.reduce((best, entry) => Math.max(best, entry.priority), 0);

  return (
    <div>
      <Head count={attention.length} />
      {error && <ErrorBlock title="Showing the last good snapshot — the refresh failed." detail={error} onRetry={refresh} />}

      <Card className="gap-b">
        <Vitality vitals={vitals.data} error={vitals.error} />
      </Card>

      <div className="grid cards-4 gap-b">
        <StatTile
          label="Queue depth"
          value={queue.length}
          note={queue.length === 0 ? "nothing eligible" : `eligible issues · top priority P${topPriority}`}
        />
        <StatTile
          label="Active workflows"
          value={active.length}
          note={`${waiting.length} waiting on you · ${spinning.length} spinning`}
        />
        <StatTile
          label="Runners online"
          value={online.length}
          suffix={`/ ${runners.length}`}
          note={runnerNote(draining.length, offline.length, stale.length)}
          noteBad={offline.length + stale.length > 0}
        />
        <StatTile
          label="Delivered"
          value={delivered.length}
          note={`${blocked.length} blocked · ${failed.length} failed`}
        />
      </div>

      <div className="grid cols-2">
        <div className="stack">
          <Card>
            <CardHeader title="Needs you" hint={plural(attention.length, "item")} />
            <div className="attn">
              {attention.length === 0
                ? <Empty>Nothing needs a decision. The Fates are spinning on their own.</Empty>
                : attention.map((item) => (
                  <AttentionItem
                    key={`${item.kind}-${item.workflow.id}`}
                    tone={item.tone}
                    title={`${projectName(item.workflow.projectId)} ${item.workflow.issueExternalId} ${item.headline}`}
                    detail={item.detail}
                    actions={
                      <Link className="btn sm primary" to={`/workflows/${item.workflow.id}`}>
                        {item.action}
                      </Link>
                    }
                  />
                ))}
            </div>
          </Card>

          <Card>
            <CardHeader
              title="In flight"
              action={<Link className="btn sm" to="/workflows">All workflows</Link>}
            />
            <div className="attn">
              {active.length === 0
                ? <Empty>No thread is being spun right now.</Empty>
                : active.map((workflow) => (
                  <AttentionItem
                    key={workflow.id}
                    tone="wait"
                    title={<>
                      <Link to={`/workflows/${workflow.id}`}>
                        {projectName(workflow.projectId)} {workflow.issueExternalId}
                      </Link>
                      <StatusPill status={workflow.status} />
                    </>}
                    detail={<>
                      {workflow.issueTitle}
                      <PhaseThread workflow={workflow} mini width={280} height={14} />
                    </>}
                  />
                ))}
            </div>
          </Card>
        </div>

        <div className="stack">
          <SystemVersions health={vitals.data?.health} runners={runners} />
          <RecentEvents api={api} workflows={active} />
        </div>
      </div>
    </div>
  );
}

function SystemVersions({ health, runners }: { health?: Health; runners: import("./api").Runner[] }) {
  const versions = [
    ["Web", import.meta.env.VITE_BUILD_VERSION],
    ["API", health?.apiVersion],
    ["Orchestrator", health?.orchestratorVersion],
    ...runners.map((runner) => [`Runner: ${runner.name}`, runner.version]),
  ].filter(([, version]) => Boolean(version?.trim())).map(([name, version]) => [name, version!.trim().slice(0, 12)]);

  return (
    <Card>
      <CardHeader title="System versions" />
      {versions.map(([name, version]) => <div className="row-line" key={name}><span>{name}</span><code>{version}</code></div>)}
    </Card>
  );
}

function Head({ count }: { count: number }) {
  return (
    <>
      <div className="view-head"><h1>Overview</h1></div>
      <p className="view-sub">
        Everything the orchestrator is spinning right now, and the {count} {count === 1 ? "thing" : "things"} that
        need a human hand.
      </p>
    </>
  );
}

function runnerNote(draining: number, offline: number, stale: number): string {
  const parts: string[] = [];
  if (draining > 0) parts.push(`${draining} draining`);
  if (offline > 0) parts.push(`${offline} offline`);
  if (stale > 0) parts.push(`${stale} not reporting`);
  return parts.length === 0 ? "whole fleet accepting offers" : parts.join(" · ");
}

// --- Health strip ---------------------------------------------------------

/**
 * The probes the control plane can answer for today. Circuits, the outbox and
 * the scheduler's own tick age arrive with specification tasks A7 and A8.
 */
function Vitality({ vitals, error }: { vitals: Vitals | null; error: string | null }) {
  if (!vitals) {
    return (
      <HealthStrip label="Orchestrator health">
        <Probe tone={error ? "crit" : "idle"} name="Orchestrator" value={error ? "unreachable" : "checking"} />
      </HealthStrip>
    );
  }

  const reachable = vitals.health.orchestrator === "reachable";
  const healthy = vitals.health.status === "healthy";
  const freshest = vitals.sync
    .map((entry) => entry.lastSyncedAt)
    .filter((value): value is string => Boolean(value))
    .sort()
    .at(-1);
  const backingOff = vitals.sync.filter((entry) => entry.backingOff);
  const eligible = vitals.sync.reduce((total, entry) => total + entry.eligibleCount, 0);

  return (
    <HealthStrip label="Orchestrator health">
      <Probe tone={reachable ? "ok" : "crit"} name="Orchestrator" value={vitals.health.orchestrator} />
      <Probe tone={healthy ? "ok" : "warn"} name="Control plane" value={vitals.health.status} />
      <Probe
        tone={backingOff.length > 0 ? "warn" : "ok"}
        name="Issue sync"
        value={backingOff.length > 0 ? `${backingOff.length} backing off` : ageAgo(freshest, "no pass yet")}
      />
      <Probe tone="ok" name="Scheduled jobs" value={vitals.metrics.scheduledJobs} />
      <Probe tone="idle" trailing value={`${eligible} eligible issues tracked`} />
    </HealthStrip>
  );
}

// --- Triage ---------------------------------------------------------------

type TriageItem = {
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

// --- Event feed -----------------------------------------------------------

type FeedEntry = { key: string; workflow: Workflow; event: WorkflowEvent };

/**
 * The console has no cross-project event stream yet (specification task E1 adds
 * one), so the feed samples the newest events of the runs currently in flight.
 */
function RecentEvents({ api, workflows }: { api: ApiClient; workflows: Workflow[] }) {
  const { projectName } = useConsoleData();
  const sampled = useMemo(
    () => [...workflows].sort((a, b) => b.updatedAt.localeCompare(a.updatedAt)).slice(0, FEED_WORKFLOWS),
    [workflows]
  );
  const ids = sampled.map((workflow) => workflow.id).join(",");

  const load = useCallback(
    async (signal: AbortSignal): Promise<FeedEntry[]> => {
      const byId = new Map(sampled.map((workflow) => [workflow.id, workflow]));
      const pages = await Promise.all(
        [...byId.keys()].map((id) =>
          api.listWorkflowEvents(id, { limit: FEED_EVENTS_PER_WORKFLOW }, signal).then((page) => ({ id, page }))
        )
      );
      return pages
        .flatMap(({ id, page }) =>
          page.events.map((event) => ({ key: `${id}:${event.id}`, workflow: byId.get(id)!, event }))
        )
        .sort((a, b) => b.event.createdAt.localeCompare(a.event.createdAt))
        .slice(0, FEED_LENGTH);
    },
    // `sampled` is rebuilt on every snapshot; the ids are what actually changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [api, ids]
  );

  const { data, error } = usePolled(load, "Recent events are unavailable.");

  return (
    <Card>
      <CardHeader title="Recent events" hint="newest across active runs" />
      {error && <Empty>{error}</Empty>}
      {!error && (
        <div className="feed">
          {(data ?? []).length === 0
            ? <Empty>No events have been recorded yet.</Empty>
            : data!.map(({ key, workflow, event }) => {
              const described = describeEvent(event);
              return (
                <FeedItem key={key} time={shortClock(event.createdAt)} tone={feedTone(workflow, described.warn)}>
                  <b>{projectName(workflow.projectId)} {workflow.issueExternalId}</b> {described.text}
                </FeedItem>
              );
            })}
        </div>
      )}
    </Card>
  );
}

function feedTone(workflow: Workflow, warn: boolean): "thread" | "ok" | "wait" | "crit" {
  if (warn || CUT_STATUSES.has(workflow.status)) return "crit";
  if (workflow.status === "completed") return "ok";
  if (isTerminal(workflow.status)) return "ok";
  return statusMeta(workflow.status).variant === "wait" ? "wait" : "thread";
}
