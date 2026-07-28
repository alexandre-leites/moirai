export type HealthStatus = "healthy" | "unhealthy";
export type Project = { id: string; name: string; enabled: boolean };
export type Runner = { id: string; name: string; enabled: boolean; draining: boolean; status: string; labels: string[]; lastSeenAt: string };
export type QueueItem = { workflowId: string; projectId: string; issueId: string; priority: number; status: string; phase: string; queuedAt: string };
export type Workflow = { id: string; projectId: string; status: string; phase: string; blockingReason?: string; planningAttempts?: number; implementationAttempts?: number; pipelineRepairAttempts?: number; reviewCycles?: number; ciRepairAttempts?: number; totalAgentExecutions?: number; pullRequestUrl?: string };
export type WorkflowEvent = { id: string; type: string; severity: string; payload: string; createdAt: string };
export type RunnerToken = { id: string; allowedLabels: string[]; expiresAt: string; usedAt?: string; revokedAt?: string };
export type CreatedToken = { token: string; expiresAt: string };
export type CurrentUser = { userId: string; username: string; role: string };
export type ControlPlaneEvent = { id: string; kind: string; resourceId: string; payload: string; createdAt: string };

export class ApiError extends Error { constructor(public status: number, message: string, public detail?: string) { super(message); this.name = "ApiError"; } }
type FetchFn = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
type ProjectInput = { name: string; repositoryMode: string; repositoryUrl?: string; localRepositoryPath?: string; defaultBranch: string; requiredRunnerLabels?: string[] };
export type ApiClient = {
  setUnauthorizedHandler(handler: (() => void) | null): void;
  health(signal?: AbortSignal): Promise<HealthStatus>; login(username: string, password: string): Promise<{ userId: string }>; logout(): Promise<void>; me(signal?: AbortSignal): Promise<CurrentUser>;
  listProjects(signal?: AbortSignal): Promise<Project[]>; createProject(data: ProjectInput): Promise<Project>; updateProject(id: string, data: ProjectInput): Promise<Project>; setProjectEnabled(id: string, enabled: boolean): Promise<Project>;
  listTokens(signal?: AbortSignal): Promise<RunnerToken[]>; createToken(allowedLabels?: string[]): Promise<CreatedToken>; revokeToken(id: string): Promise<void>;
  listWorkflows(signal?: AbortSignal): Promise<Workflow[]>; getWorkflow(id: string, signal?: AbortSignal): Promise<{ workflow: Workflow; events: WorkflowEvent[] }>; submitWorkflowDecision(id: string, decision: "approved" | "changes_requested", comment?: string): Promise<Workflow>; workflowAction(id: string, action: "retry" | "cancel" | "block", reason?: string): Promise<Workflow>;
  listRunners(signal?: AbortSignal): Promise<Runner[]>; setRunnerState(id: string, state: "enable" | "disable" | "drain" | "revoke"): Promise<Runner>; listQueue(signal?: AbortSignal): Promise<QueueItem[]>;
};
const CSRF_COOKIE_NAME = "loop_csrf";
const getCSRF = (): string | null => { const match = document.cookie.match(new RegExp(`(?:^|;\\s*)${CSRF_COOKIE_NAME}=([^;]+)`)); return match ? decodeURIComponent(match[1]) : null; };
const csrfHeaders = (): Record<string, string> => { const token = getCSRF(); return token ? { "x-csrf-token": token } : {}; };
export function createApiClient(fetchClient: FetchFn = fetch): ApiClient {
  let unauthorizedHandler: (() => void) | null = null;
  const json = async <T,>(res: Response): Promise<T> => { if (!res.ok) { if (res.status === 401) unauthorizedHandler?.(); let title = `request failed: ${res.status}`; let detail: string | undefined; try { const body: unknown = await res.json(); if (body && typeof body === "object") { const p = body as { title?: unknown; detail?: unknown }; if (typeof p.title === "string") title = p.title; if (typeof p.detail === "string") detail = p.detail; } } catch { return Promise.reject(new ApiError(res.status, title)); } throw new ApiError(res.status, detail ? `${title}: ${detail}` : title, detail); } return res.json() as Promise<T>; };
  const get = <T,>(path: string, signal?: AbortSignal) => fetchClient(path, { signal, credentials: "include" }).then(json<T>);
  const mutate = <T,>(path: string, method: string, body?: unknown) => fetchClient(path, { method, headers: { ...(body === undefined ? {} : { "Content-Type": "application/json" }), ...csrfHeaders() }, credentials: "include", body: body === undefined ? undefined : JSON.stringify(body) }).then(json<T>);
  return {
    setUnauthorizedHandler(handler) { unauthorizedHandler = handler; }, health: async signal => (await fetchClient("/api/v1/health", { signal })).ok ? "healthy" : "unhealthy",
    login: (username, password) => mutate("/api/v1/auth/login", "POST", { username, password }), logout: () => mutate("/api/v1/auth/logout", "POST"), me: signal => get("/api/v1/auth/me", signal),
    listProjects: async signal => (await get<{ projects: Project[] }>("/api/v1/projects", signal)).projects, createProject: data => mutate("/api/v1/projects", "POST", data), updateProject: (id, data) => mutate(`/api/v1/projects/${id}`, "PUT", data), setProjectEnabled: (id, enabled) => mutate(`/api/v1/projects/${id}/${enabled ? "enable" : "disable"}`, "POST"),
    listTokens: async signal => (await get<{ tokens: RunnerToken[] }>("/api/v1/runner-tokens", signal)).tokens, createToken: labels => mutate("/api/v1/runner-tokens", "POST", { allowedLabels: labels ?? [] }), revokeToken: id => mutate(`/api/v1/runner-tokens/${id}`, "DELETE"),
    listWorkflows: async signal => (await get<{ workflows: Workflow[] }>("/api/v1/workflows", signal)).workflows, getWorkflow: (id, signal) => get(`/api/v1/workflows/${id}`, signal), submitWorkflowDecision: (id, decision, comment) => mutate(`/api/v1/workflows/${id}/decision`, "POST", { decision, comment: comment ?? "" }), workflowAction: (id, action, reason) => mutate(`/api/v1/workflows/${id}/${action}`, "POST", { reason: reason ?? "" }),
    listRunners: async signal => (await get<{ runners: Runner[] }>("/api/v1/runners", signal)).runners, setRunnerState: (id, state) => mutate(`/api/v1/runners/${id}/state`, "POST", { state }), listQueue: async signal => (await get<{ items: QueueItem[] }>("/api/v1/queue", signal)).items,
  };
}
