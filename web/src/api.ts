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
