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

type FetchFn = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

export type ApiClient = {
  health(signal?: AbortSignal): Promise<HealthStatus>;

  login(username: string, password: string): Promise<{ userId: string }>;
  logout(): Promise<void>;

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

const getCSRF = (): string | null => {
  const match = document.cookie.match(/(?:^|;\s*)csrf=([^;]+)/);
  return match ? decodeURIComponent(match[1]) : null;
};

function csrfHeaders(): Record<string, string> {
  const token = getCSRF();
  if (token) return { "x-csrf-token": token };
  return {};
}

export function createApiClient(fetchClient: FetchFn = fetch): ApiClient {
  const json = (res: Response) => {
    if (!res.ok) throw new Error(`request failed: ${res.status}`);
    return res.json();
  };
  return {
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
      });
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
