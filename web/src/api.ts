// Typed client for the Go management API (`/api/v1`), which proxies the
// orchestrator's gRPC control plane. Response shapes mirror api/openapi.yaml;
// errors are RFC-7807 problem+json.

export type HealthStatus = "healthy" | "unhealthy";

/** `GET /api/v1/health` — the API's own view of the control plane. */
export type Health = {
  status: string;
  orchestrator: string;
};

// Mirrors the `Project` schema in api/openapi.yaml, served by
// GET /api/v1/projects (api/internal/http/handlers/handlers.go). All
// configuration fields are always present: the handler marshals the
// orchestrator's project record verbatim, so an absent field on the wire is a
// broken response. `requiredRunnerLabels` is always an array here — the
// handler normalizes the protobuf's nil slice to `[]`.
export type PipelineStep = {
  command: string;
  timeoutSeconds: number;
  position: number;
  required: boolean;
};

export type Project = {
  id: string;
  name: string;
  enabled: boolean;
  repositoryMode: string;
  repositoryUrl: string;
  localRepositoryPath: string;
  defaultBranch: string;
  requiredRunnerLabels: string[];
  pipelineSteps: PipelineStep[];
};

type ProjectPayload = Omit<Project, "requiredRunnerLabels" | "pipelineSteps"> & {
  requiredRunnerLabels: string[] | null;
  pipelineSteps: PipelineStep[] | null;
};

/**
 * One workflow run. `GET /api/v1/workflows` and `GET /api/v1/workflows/{id}`
 * answer with this same shape, so the list view needs no per-row detail fetch.
 *
 * `status` is the scheduling lifecycle and `phase` the graph node the run
 * committed to; they agree most of the time but diverge while a job is being
 * placed or recovered. See "Run status versus run phase" in orchestrator/README.md.
 */
export type Workflow = {
  id: string;
  projectId: string;
  status: string;
  phase: string;
  issueExternalId: string;
  issueTitle: string;
  branchName: string;
  pullRequestExternalId: string;
  pullRequestUrl: string;
  pullRequestState: string;
  blockingReason: string;
  planningAttempts: number;
  implementationAttempts: number;
  pipelineRepairAttempts: number;
  ciRepairAttempts: number;
  reviewCycles: number;
  totalAgentExecutions: number;
  createdAt: string;
  updatedAt: string;
};

/**
 * What the control endpoints (retry/cancel/block/decision) answer with. Those
 * responses are built from the resumed graph state rather than a row read, so
 * only the lifecycle fields are populated — see workflowPayload in
 * api/internal/http/handlers/workflows.go.
 */
export type WorkflowLifecycle = Pick<Workflow, "id" | "projectId" | "status" | "phase">;

export type WorkflowEvent = {
  id: string;
  type: string;
  createdAt: string;
  payload: unknown;
};

export type WorkflowEventsPage = {
  events: WorkflowEvent[];
  nextCursor?: string;
};

/** `cursor` pages backwards through history; `limit` caps one page (max 100). */
export type EventPageOptions = {
  cursor?: string;
  limit?: number;
};

// Mirrors the `Runner` schema in api/openapi.yaml, served by
// GET /api/v1/runners (api/internal/http/handlers/runners.go). `status` is the
// orchestrator's own column, currently "online" or "offline"; it is typed as a
// plain string so a value added server-side does not break the client.
// `lastSeenAt` is an ISO-8601 timestamp with an offset, or "" when the runner
// has never reported a heartbeat. `labels` is always an array here — see
// `listRunners`, which normalizes the wire's `null`.
export type Runner = {
  id: string;
  name: string;
  enabled: boolean;
  draining: boolean;
  status: string;
  labels: string[];
  lastSeenAt: string;
};

// What the wire actually carries. The handler marshals the protobuf's repeated
// `labels` field straight to JSON, and an empty repeated field is a nil slice
// in Go, which encoding/json writes as `null` rather than `[]`.
type RunnerPayload = Omit<Runner, "labels"> & { labels: string[] | null };

export type RunnerToken = {
  id: string;
  allowedLabels: string[];
  expiresAt: string;
  usedAt?: string;
  revokedAt?: string;
};

type RunnerTokenPayload = Omit<RunnerToken, "allowedLabels"> & { allowedLabels: string[] | null };

export type CreatedToken = {
  token: string;
  expiresAt: string;
};

export type CurrentUser = {
  userId: string;
  username: string;
  role: string;
  email: string;
  displayName: string;
};

export type QueueEntry = {
  projectId: string;
  projectName: string;
  externalId: string;
  title: string;
  priority: number;
  blockedReason: string;
};

/** `GET /api/v1/sync/status` — per-project issue synchronization state. */
export type IssueSyncStatus = {
  projectId: string;
  projectName: string;
  enabled: boolean;
  issueCount: number;
  eligibleCount: number;
  lastSyncedAt?: string;
  consecutiveFailures: number;
  nextRetryAt?: string;
  lastError?: string;
  backingOff: boolean;
};

export type SyncResult = {
  projectId: string;
  syncedIssues: number;
  error?: string;
};

export type SchedulerMetrics = {
  queueDepth: number;
  activeWorkflows: number;
  scheduledJobs: number;
};

/**
 * What a project has configured, never the value. There is deliberately no
 * endpoint that returns a credential: it travels inbound only.
 */
export type ProjectCredential = {
  kind: CredentialKind;
  createdAt: string;
  updatedAt: string;
};

export type CredentialKind = "github_token" | "ssh_private_key";

export type ProjectConfiguration = {
  name: string;
  repositoryMode: string;
  repositoryUrl?: string;
  localRepositoryPath?: string;
  defaultBranch: string;
  requiredRunnerLabels?: string[];
  pipelineSteps?: PipelineStep[];
};

export class ApiError extends Error {
  status: number;
  detail?: string;

  constructor(status: number, message: string, detail?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.detail = detail;
  }

  /** 403 means "signed in, but not allowed" — the caller stays where it is. */
  get isForbidden(): boolean {
    return this.status === 403;
  }
}

type FetchFn = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

export type ApiClient = {
  // Registers a callback invoked whenever a request comes back 401 Unauthorized,
  // so callers (AuthProvider) can treat "session gone" uniformly instead of every
  // page having to interpret a thrown ApiError itself. Pass null to unregister.
  //
  // 403 is deliberately not routed here: a viewer who clicks an admin-only
  // control has a perfectly good session and must not be signed out for it
  // (specification.md §3.2).
  setUnauthorizedHandler(handler: (() => void) | null): void;

  health(signal?: AbortSignal): Promise<Health>;

  login(username: string, password: string): Promise<{ userId: string }>;
  logout(): Promise<void>;
  me(signal?: AbortSignal): Promise<CurrentUser>;
  updateAccount(data: {
    currentPassword?: string;
    newPassword?: string;
    newEmail?: string;
    displayName?: string;
  }): Promise<CurrentUser>;

  listProjects(signal?: AbortSignal): Promise<Project[]>;
  createProject(data: ProjectConfiguration): Promise<Project>;
  updateProject(id: string, data: ProjectConfiguration): Promise<Project>;
  setProjectEnabled(id: string, enabled: boolean): Promise<Project>;
  listProjectCredentials(id: string, signal?: AbortSignal): Promise<ProjectCredential[]>;
  setProjectCredential(id: string, kind: CredentialKind, value: string): Promise<ProjectCredential[]>;
  clearProjectCredential(id: string, kind: CredentialKind): Promise<ProjectCredential[]>;

  listRunners(signal?: AbortSignal): Promise<Runner[]>;
  setRunnerState(id: string, state: "drain" | "enable" | "revoke"): Promise<Runner>;

  listTokens(signal?: AbortSignal): Promise<RunnerToken[]>;
  createToken(allowedLabels?: string[]): Promise<CreatedToken>;
  revokeToken(id: string): Promise<void>;

  listWorkflows(signal?: AbortSignal): Promise<Workflow[]>;
  getWorkflow(id: string, signal?: AbortSignal): Promise<Workflow>;
  listWorkflowEvents(id: string, options?: EventPageOptions, signal?: AbortSignal): Promise<WorkflowEventsPage>;
  submitWorkflowDecision(id: string, decision: "approved" | "changes_requested", comment?: string): Promise<WorkflowLifecycle>;
  retryWorkflow(id: string, reason?: string): Promise<WorkflowLifecycle>;
  cancelWorkflow(id: string, reason?: string): Promise<WorkflowLifecycle>;
  blockWorkflow(id: string, reason: string): Promise<WorkflowLifecycle>;

  listQueue(signal?: AbortSignal, limit?: number): Promise<QueueEntry[]>;
  schedulerMetrics(signal?: AbortSignal): Promise<SchedulerMetrics>;

  issueSyncStatus(signal?: AbortSignal): Promise<IssueSyncStatus[]>;
  // Runs an issue-synchronization pass now. projectId is optional; when given
  // only that project syncs, otherwise every enabled project syncs.
  syncNow(projectId?: string): Promise<SyncResult[]>;
};

// CSRF_COOKIE_NAME must match auth.CSRFCookieName in api/internal/auth/session.go —
// that is the cookie the server actually sets. See session_web_test.go for the
// regression test guarding this constant against the two names drifting apart.
const CSRF_COOKIE_NAME = "loop_csrf";

const getCSRF = (): string | null => {
  const match = document.cookie.match(
    new RegExp(`(?:^|;\\s*)${CSRF_COOKIE_NAME}=([^;]+)`)
  );
  return match ? decodeURIComponent(match[1]) : null;
};

function csrfHeaders(): Record<string, string> {
  const token = getCSRF();
  if (token) return { "x-csrf-token": token };
  return {};
}

export function createApiClient(fetchClient: FetchFn = fetch): ApiClient {
  let unauthorizedHandler: (() => void) | null = null;

  /** Turns a non-2xx response into an ApiError carrying its problem+json detail. */
  const failure = async (res: Response): Promise<ApiError> => {
    if (res.status === 401) unauthorizedHandler?.();
    let title = `request failed: ${res.status}`;
    let detail: string | undefined;
    try {
      const body = await res.json();
      if (body && typeof body === "object") {
        if (typeof body.title === "string" && body.title) title = body.title;
        if (typeof body.detail === "string" && body.detail) detail = body.detail;
      }
    } catch {
      // Response body was not JSON (or was empty) — fall back to the generic message.
    }
    return new ApiError(res.status, detail ? `${title}: ${detail}` : title, detail);
  };

  const json = async (res: Response) => {
    if (!res.ok) throw await failure(res);
    return res.json();
  };

  const get = async (path: string, signal?: AbortSignal) =>
    json(await fetchClient(path, { signal, credentials: "include" }));

  const post = async (path: string, body?: unknown) =>
    json(await fetchClient(path, {
      method: "POST",
      headers: body === undefined ? csrfHeaders() : { "Content-Type": "application/json", ...csrfHeaders() },
      credentials: "include",
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    }));

  const controlWorkflow = async (
    id: string,
    action: "retry" | "cancel" | "block",
    reason?: string
  ): Promise<WorkflowLifecycle> =>
    post(`/api/v1/workflows/${encodeURIComponent(id)}/${action}`, { reason: reason ?? "" });

  return {
    setUnauthorizedHandler(handler: (() => void) | null): void {
      unauthorizedHandler = handler;
    },

    // A degraded control plane answers 503 with the very body this view needs
    // ("degraded" / "unreachable"), so the status code is not a reason to throw
    // it away — see the /health handler in api/internal/http/server.go. Only a
    // response with no usable body reports unreachable on its own.
    async health(signal?: AbortSignal): Promise<Health> {
      const res = await fetchClient("/api/v1/health", { signal });
      try {
        const body: Partial<Health> = await res.json();
        if (typeof body?.status === "string" && typeof body?.orchestrator === "string") {
          return { status: body.status, orchestrator: body.orchestrator };
        }
      } catch {
        // Not JSON: fall through to the status-code reading below.
      }
      return res.ok
        ? { status: "healthy", orchestrator: "reachable" }
        : { status: "degraded", orchestrator: "unreachable" };
    },

    async login(username: string, password: string): Promise<{ userId: string }> {
      const res = await fetchClient("/api/v1/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      return json(res);
    },

    async logout(): Promise<void> {
      await fetchClient("/api/v1/auth/logout", {
        method: "POST",
        headers: { ...csrfHeaders() },
        credentials: "include",
      });
    },

    async me(signal?: AbortSignal): Promise<CurrentUser> {
      return get("/api/v1/auth/me", signal);
    },

    async updateAccount(data: {
      currentPassword?: string;
      newPassword?: string;
      newEmail?: string;
      displayName?: string;
    }): Promise<CurrentUser> {
      const res = await fetchClient("/api/v1/auth/account", {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...csrfHeaders() },
        credentials: "include",
        body: JSON.stringify(data),
      });
      return json(res);
    },

    async listProjects(signal?: AbortSignal): Promise<Project[]> {
      const body: { projects?: ProjectPayload[] } = await get("/api/v1/projects", signal);
      if (!Array.isArray(body.projects)) throw new Error("The project list response was malformed.");
      return body.projects.map(normalizeProject);
    },

    async createProject(data: ProjectConfiguration): Promise<Project> {
      return normalizeProject(await post("/api/v1/projects", data));
    },

    async updateProject(id: string, data: ProjectConfiguration): Promise<Project> {
      const res = await fetchClient(`/api/v1/projects/${encodeURIComponent(id)}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...csrfHeaders() },
        credentials: "include",
        body: JSON.stringify(data),
      });
      return normalizeProject(await json(res));
    },

    async setProjectEnabled(id: string, enabled: boolean): Promise<Project> {
      const endpoint = enabled ? "enable" : "disable";
      return normalizeProject(await post(`/api/v1/projects/${encodeURIComponent(id)}/${endpoint}`));
    },

    async listProjectCredentials(id: string, signal?: AbortSignal): Promise<ProjectCredential[]> {
      const body: { credentials?: ProjectCredential[] } = await get(
        `/api/v1/projects/${encodeURIComponent(id)}/credentials`, signal
      );
      if (!Array.isArray(body.credentials)) throw new Error("The credential response was malformed.");
      return body.credentials;
    },

    async setProjectCredential(id: string, kind: CredentialKind, value: string): Promise<ProjectCredential[]> {
      const res = await fetchClient(
        `/api/v1/projects/${encodeURIComponent(id)}/credentials/${encodeURIComponent(kind)}`,
        {
          method: "PUT",
          headers: { "Content-Type": "application/json", ...csrfHeaders() },
          credentials: "include",
          body: JSON.stringify({ value }),
        }
      );
      const body: { credentials?: ProjectCredential[] } = await json(res);
      return body.credentials ?? [];
    },

    async clearProjectCredential(id: string, kind: CredentialKind): Promise<ProjectCredential[]> {
      const res = await fetchClient(
        `/api/v1/projects/${encodeURIComponent(id)}/credentials/${encodeURIComponent(kind)}`,
        { method: "DELETE", headers: { ...csrfHeaders() }, credentials: "include" }
      );
      const body: { credentials?: ProjectCredential[] } = await json(res);
      return body.credentials ?? [];
    },

    async listRunners(signal?: AbortSignal): Promise<Runner[]> {
      const body: { runners?: RunnerPayload[] } = await get("/api/v1/runners", signal);
      // `runners` is required by the OpenAPI schema. If it is missing the
      // response is not the one we asked for, and returning [] here would
      // render the "no runner is registered" empty state for what is really a
      // broken response — the exact silent failure the runners view must not
      // have. Not an ApiError: the server answered 200, this is our own read of
      // the body failing, and claiming an HTTP status it never sent would lie.
      if (!Array.isArray(body.runners)) {
        throw new Error("The runner list response was malformed.");
      }
      return body.runners.map((runner) => ({ ...runner, labels: runner.labels ?? [] }));
    },

    async setRunnerState(id: string, state: "drain" | "enable" | "revoke"): Promise<Runner> {
      const action = state === "enable" ? "undrain" : state;
      const runner: RunnerPayload = await post(`/api/v1/runners/${encodeURIComponent(id)}/${action}`);
      return { ...runner, labels: runner.labels ?? [] };
    },

    async listTokens(signal?: AbortSignal): Promise<RunnerToken[]> {
      const body: { tokens?: RunnerTokenPayload[] } = await get("/api/v1/runner-tokens", signal);
      if (!Array.isArray(body.tokens)) throw new Error("The token list response was malformed.");
      return body.tokens.map((token) => ({ ...token, allowedLabels: token.allowedLabels ?? [] }));
    },

    async createToken(allowedLabels?: string[]): Promise<CreatedToken> {
      return post("/api/v1/runner-tokens", { allowedLabels: allowedLabels ?? [] });
    },

    async revokeToken(id: string): Promise<void> {
      const res = await fetchClient(`/api/v1/runner-tokens/${encodeURIComponent(id)}`, {
        method: "DELETE",
        headers: { ...csrfHeaders() },
        credentials: "include",
      });
      // 204 with no body: there is nothing to parse, only a status to check.
      if (!res.ok) throw await failure(res);
    },

    async listWorkflows(signal?: AbortSignal): Promise<Workflow[]> {
      const body: { workflows?: Workflow[] } = await get("/api/v1/workflows", signal);
      if (!Array.isArray(body.workflows)) throw new Error("The workflow list response was malformed.");
      return body.workflows;
    },

    async getWorkflow(id: string, signal?: AbortSignal): Promise<Workflow> {
      return get(`/api/v1/workflows/${encodeURIComponent(id)}`, signal);
    },

    async listWorkflowEvents(id: string, options: EventPageOptions = {}, signal?: AbortSignal): Promise<WorkflowEventsPage> {
      const query = new URLSearchParams();
      if (options.cursor) query.set("cursor", options.cursor);
      if (options.limit) query.set("limit", String(options.limit));
      const suffix = query.size > 0 ? `?${query}` : "";
      return get(`/api/v1/workflows/${encodeURIComponent(id)}/events${suffix}`, signal);
    },

    async submitWorkflowDecision(
      id: string,
      decision: "approved" | "changes_requested",
      comment?: string
    ): Promise<WorkflowLifecycle> {
      return post(`/api/v1/workflows/${encodeURIComponent(id)}/decision`, { decision, comment: comment ?? "" });
    },

    async retryWorkflow(id: string, reason?: string): Promise<WorkflowLifecycle> {
      return controlWorkflow(id, "retry", reason);
    },

    async cancelWorkflow(id: string, reason?: string): Promise<WorkflowLifecycle> {
      return controlWorkflow(id, "cancel", reason);
    },

    async blockWorkflow(id: string, reason: string): Promise<WorkflowLifecycle> {
      return controlWorkflow(id, "block", reason);
    },

    async listQueue(signal?: AbortSignal, limit?: number): Promise<QueueEntry[]> {
      const query = limit ? `?limit=${limit}` : "";
      const body: { entries?: QueueEntry[] } = await get(`/api/v1/queue${query}`, signal);
      if (!Array.isArray(body.entries)) throw new Error("The queue response was malformed.");
      return body.entries;
    },

    async schedulerMetrics(signal?: AbortSignal): Promise<SchedulerMetrics> {
      return get("/api/v1/scheduler/metrics", signal);
    },

    async issueSyncStatus(signal?: AbortSignal): Promise<IssueSyncStatus[]> {
      const body: { entries?: IssueSyncStatus[] } = await get("/api/v1/sync/status", signal);
      if (!Array.isArray(body.entries)) throw new Error("The issue sync response was malformed.");
      return body.entries;
    },

    async syncNow(projectId?: string): Promise<SyncResult[]> {
      const body: { results?: SyncResult[] } = await post("/api/v1/sync", projectId ? { projectId } : {});
      return body.results ?? [];
    },
  };
}

function normalizeProject(project: ProjectPayload): Project {
  return {
    ...project,
    requiredRunnerLabels: project.requiredRunnerLabels ?? [],
    pipelineSteps: project.pipelineSteps ?? [],
  };
}
