// Mounting helpers for view tests: a stub ApiClient plus the providers every
// view expects (router, auth, toasts, and the shared console snapshot).
//
// Deliberately not named `*.test.tsx` — vitest's `include` would collect it as a
// suite of its own and fail it for having no tests.
import type { ReactElement } from "react";
import { MemoryRouter, Route, Routes } from "react-router";
import type {
  ApiClient, CurrentUser, Project, ProjectCredential, QueueEntry, Runner, RunnerToken,
  Workflow, WorkflowEvent,
} from "./api";
import { AuthProvider } from "./auth";
import { ConsoleDataProvider } from "./console-data";
import { ToastProvider } from "./ui";
import { mount } from "./test-dom";

export const NOW = "2026-08-01T12:00:00Z";

export function workflow(overrides: Partial<Workflow> = {}): Workflow {
  return {
    id: "wf-1",
    projectId: "project-1",
    status: "preparing",
    phase: "preparing",
    issueExternalId: "#103",
    issueTitle: "Close execution requests on terminal transitions",
    branchName: "agent/103-close-execution-requests",
    pullRequestExternalId: "",
    pullRequestUrl: "",
    pullRequestState: "",
    blockingReason: "",
    planningAttempts: 1,
    implementationAttempts: 2,
    pipelineRepairAttempts: 0,
    ciRepairAttempts: 0,
    reviewCycles: 0,
    totalAgentExecutions: 3,
    createdAt: NOW,
    updatedAt: NOW,
    ...overrides,
  };
}

export function project(overrides: Partial<Project> = {}): Project {
  return {
    id: "project-1",
    name: "moirai",
    enabled: true,
    repositoryMode: "managed_clone",
    repositoryUrl: "github.com/williamokano/moirai",
    localRepositoryPath: "",
    defaultBranch: "main",
    requiredRunnerLabels: ["go"],
    pipelineSteps: [],
    executionImage: "",
    requireHumanApproval: false,
    ...overrides,
  };
}

export function runner(overrides: Partial<Runner> = {}): Runner {
  return {
    id: "00000000-0000-0000-0000-0000000000a1",
    name: "loom-01",
    enabled: true,
    draining: false,
    status: "online",
    version: "",
    labels: ["go", "docker"],
    lastSeenAt: new Date().toISOString(),
    ...overrides,
  };
}

export function queueEntry(overrides: Partial<QueueEntry> = {}): QueueEntry {
  return {
    projectId: "project-1",
    projectName: "moirai",
    externalId: "#104",
    title: "Expose workflow events over the management API",
    priority: 5,
    blockedReason: "project_locked",
    ...overrides,
  };
}

export function token(overrides: Partial<RunnerToken> = {}): RunnerToken {
  return {
    id: "tok-31ac",
    allowedLabels: ["go"],
    expiresAt: NOW,
    ...overrides,
  };
}

export function credential(overrides: Partial<ProjectCredential> = {}): ProjectCredential {
  return { kind: "github_token", createdAt: NOW, updatedAt: NOW, filePath: "", ...overrides };
}

export function event(overrides: Partial<WorkflowEvent> = {}): WorkflowEvent {
  return {
    id: "10",
    type: "workflow_transition",
    createdAt: NOW,
    payload: { status: "preparing" },
    ...overrides,
  };
}

export const ADMIN: CurrentUser = {
  userId: "u-1", username: "william", role: "admin", email: "w@example.test", displayName: "William",
};

export const VIEWER: CurrentUser = { ...ADMIN, userId: "u-2", username: "vera", role: "viewer" };

/**
 * A client whose every method resolves to an empty result, so a test only has to
 * say what it cares about. Any method a test forgets to stub still resolves
 * rather than throwing an unrelated failure at the view.
 */
export function stubApi(overrides: Partial<ApiClient> = {}): ApiClient {
  const base: ApiClient = {
    setUnauthorizedHandler: () => undefined,
    health: async () => ({ status: "healthy", orchestrator: "reachable" }),
    login: async () => ({ userId: ADMIN.userId }),
    logout: async () => undefined,
    me: async () => ADMIN,
    updateAccount: async () => ADMIN,
    listProjects: async () => [],
    createProject: async () => project(),
    updateProject: async () => project(),
    setProjectEnabled: async () => project(),
    listProjectCredentials: async () => [],
    setProjectCredential: async () => [],
    clearProjectCredential: async () => [],
    listRunners: async () => [],
    setRunnerState: async () => runner(),
    listTokens: async () => [],
    createToken: async () => ({ token: "moi_secret", expiresAt: NOW }),
    revokeToken: async () => undefined,
    listWorkflows: async () => [],
    getWorkflow: async () => workflow(),
    listWorkflowEvents: async () => ({ events: [] }),
    submitWorkflowDecision: async () => workflow(),
    retryWorkflow: async () => workflow(),
    cancelWorkflow: async () => workflow(),
    blockWorkflow: async () => workflow(),
    listQueue: async () => [],
    schedulerMetrics: async () => ({ queueDepth: 0, activeWorkflows: 0, scheduledJobs: 0, loops: [] }),
    issueSyncStatus: async () => [],
    syncNow: async () => [],
  };
  return { ...base, ...overrides };
}

export type MountOptions = {
  /** Initial URL; use with `path` for views that read route params. */
  route?: string;
  /** Route pattern the element is mounted at, when it takes params. */
  path?: string;
};

/** Mounts one view with every provider the console gives it in production. */
export async function mountView(
  element: ReactElement,
  api: ApiClient,
  { route = "/", path = "*" }: MountOptions = {}
): Promise<HTMLElement> {
  return mount(
    <MemoryRouter initialEntries={[route]}>
      <AuthProvider api={api}>
        <ToastProvider>
          <ConsoleDataProvider api={api}>
            <Routes>
              <Route path={path} element={element} />
            </Routes>
          </ConsoleDataProvider>
        </ToastProvider>
      </AuthProvider>
    </MemoryRouter>
  );
}
