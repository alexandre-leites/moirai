export type HealthStatus = "healthy" | "unhealthy";

export type Project = {
  id: string;
  name: string;
  enabled: boolean;
};

export type Workflow = {
  id: string;
  projectId: string;
  status: string;
  phase: string;
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

export type CreatedToken = {
  token: string;
  expiresAt: string;
};

export type CurrentUser = {
  userId: string;
  username: string;
  role: string;
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
}

type FetchFn = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

export type ApiClient = {
  // Registers a callback invoked whenever a request comes back 401 Unauthorized,
  // so callers (AuthProvider) can treat "session gone" uniformly instead of every
  // page having to interpret a thrown ApiError itself. Pass null to unregister.
  setUnauthorizedHandler(handler: (() => void) | null): void;

  health(signal?: AbortSignal): Promise<HealthStatus>;

  login(username: string, password: string): Promise<{ userId: string }>;
  logout(): Promise<void>;
  me(signal?: AbortSignal): Promise<CurrentUser>;

  listProjects(signal?: AbortSignal): Promise<Project[]>;
  createProject(data: {
    name: string;
    repositoryMode: string;
    repositoryUrl?: string;
    localRepositoryPath?: string;
    defaultBranch: string;
    requiredRunnerLabels?: string[];
  }): Promise<Project>;
  updateProject(id: string, data: {
    name: string;
    repositoryMode: string;
    repositoryUrl?: string;
    localRepositoryPath?: string;
    defaultBranch: string;
    requiredRunnerLabels?: string[];
  }): Promise<Project>;
  setProjectEnabled(id: string, enabled: boolean): Promise<Project>;

  listRunners(signal?: AbortSignal): Promise<Runner[]>;

  listTokens(signal?: AbortSignal): Promise<RunnerToken[]>;
  createToken(allowedLabels?: string[]): Promise<CreatedToken>;
  revokeToken(id: string): Promise<void>;

  listWorkflows(signal?: AbortSignal): Promise<Workflow[]>;
  submitWorkflowDecision(id: string, decision: "approved" | "changes_requested", comment?: string): Promise<Workflow>;
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
  const json = async (res: Response) => {
    if (!res.ok) {
      if (res.status === 401) unauthorizedHandler?.();
      let title = `request failed: ${res.status}`;
      let detail: string | undefined;
      try {
        const body = await res.json();
        if (body && typeof body === "object") {
          if (typeof body.title === "string") title = body.title;
          if (typeof body.detail === "string" && body.detail) detail = body.detail;
        }
      } catch {
        // Response body was not JSON (or was empty) — fall back to the generic message.
      }
      throw new ApiError(res.status, detail ? `${title}: ${detail}` : title, detail);
    }
    return res.json();
  };
  return {
    setUnauthorizedHandler(handler: (() => void) | null): void {
      unauthorizedHandler = handler;
    },

    async health(signal?: AbortSignal): Promise<HealthStatus> {
      const res = await fetchClient("/api/v1/health", { signal });
      return res.ok ? "healthy" : "unhealthy";
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
      const res = await fetchClient("/api/v1/auth/me", { signal, credentials: "include" });
      return json(res);
    },

    async listProjects(signal?: AbortSignal): Promise<Project[]> {
      const res = await fetchClient("/api/v1/projects", { signal, credentials: "include" });
      const body: { projects: Project[] } = await json(res);
      return body.projects;
    },

    async createProject(data: {
      name: string;
      repositoryMode: string;
      repositoryUrl?: string;
      localRepositoryPath?: string;
      defaultBranch: string;
      requiredRunnerLabels?: string[];
    }): Promise<Project> {
      const res = await fetchClient("/api/v1/projects", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...csrfHeaders() },
        credentials: "include",
        body: JSON.stringify(data),
      });
      const body: Project = await json(res);
      return body;
    },

    async updateProject(id: string, data: {
      name: string;
      repositoryMode: string;
      repositoryUrl?: string;
      localRepositoryPath?: string;
      defaultBranch: string;
      requiredRunnerLabels?: string[];
    }): Promise<Project> {
      const res = await fetchClient(`/api/v1/projects/${id}`, {
        method: "PUT",
        headers: { "Content-Type": "application/json", ...csrfHeaders() },
        credentials: "include",
        body: JSON.stringify(data),
      });
      const body: Project = await json(res);
      return body;
    },

    async setProjectEnabled(id: string, enabled: boolean): Promise<Project> {
      const endpoint = enabled ? "enable" : "disable";
      const res = await fetchClient(`/api/v1/projects/${id}/${endpoint}`, {
        method: "POST",
        headers: { ...csrfHeaders() },
        credentials: "include",
      });
      const body: Project = await json(res);
      return body;
    },

    async listRunners(signal?: AbortSignal): Promise<Runner[]> {
      const res = await fetchClient("/api/v1/runners", { signal, credentials: "include" });
      const body: { runners?: RunnerPayload[] } = await json(res);
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

    async listTokens(signal?: AbortSignal): Promise<RunnerToken[]> {
      const res = await fetchClient("/api/v1/runner-tokens", { signal, credentials: "include" });
      const body: { tokens: RunnerToken[] } = await json(res);
      return body.tokens;
    },

    async createToken(allowedLabels?: string[]): Promise<CreatedToken> {
      const res = await fetchClient("/api/v1/runner-tokens", {
        method: "POST",
        headers: { "Content-Type": "application/json", ...csrfHeaders() },
        credentials: "include",
        body: JSON.stringify({ allowedLabels: allowedLabels ?? [] }),
      });
      return json(res);
    },

    async revokeToken(id: string): Promise<void> {
      await fetchClient(`/api/v1/runner-tokens/${id}`, {
        method: "DELETE",
        headers: { ...csrfHeaders() },
        credentials: "include",
      });
    },

    async listWorkflows(signal?: AbortSignal): Promise<Workflow[]> {
      const res = await fetchClient("/api/v1/workflows", { signal, credentials: "include" });
      const body: { workflows: Workflow[] } = await json(res);
      return body.workflows;
    },

    async submitWorkflowDecision(
      id: string,
      decision: "approved" | "changes_requested",
      comment?: string
    ): Promise<Workflow> {
      const res = await fetchClient(`/api/v1/workflows/${id}/decision`, {
        method: "POST",
        headers: { "Content-Type": "application/json", ...csrfHeaders() },
        credentials: "include",
        body: JSON.stringify({ decision, comment: comment ?? "" }),
      });
      return json(res);
    },
  };
}
