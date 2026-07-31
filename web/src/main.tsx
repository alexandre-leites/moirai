import { Component, useEffect, useState, type ErrorInfo, type ReactNode } from "react";
import { BrowserRouter, Routes, Route, Navigate, NavLink, Link, useLocation } from "react-router-dom";
import { createRoot } from "react-dom/client";
import { createApiClient } from "./api";
import type { ApiClient, Project, Runner, Workflow } from "./api";
import { AuthProvider, useAuth, useIsAdmin } from "./auth";
import { loadHealth } from "./health";
import type { HealthViewState } from "./health";
import { LoginPage } from "./login";
import { ProjectsPage } from "./projects";
import { QueuePage } from "./queue";
import { RunnersPage } from "./runners";
import { TokensPage } from "./tokens";
import { WorkflowsPage } from "./workflows";
import { WorkflowDetailPage } from "./workflow-detail";
import "./styles.css";

const api = createApiClient();

class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state: { error: Error | null } = { error: null };

  static getDerivedStateFromError(error: Error): { error: Error } {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error("Unhandled UI error", error, info.componentStack);
  }

  render(): ReactNode {
    if (this.state.error) {
      return <div className="error-boundary"><h1>Something went wrong</h1><p>{this.state.error.message}</p><button onClick={() => this.setState({ error: null })}>Try again</button></div>;
    }
    return this.props.children;
  }
}

function ProtectedRoute({ children }: { children: ReactNode }) {
  const { state, loading } = useAuth();
  if (loading) return <p className="route-loading">Loading console...</p>;
  if (!state) return <Navigate to="/login" replace />;
  return <>{children}</>;
}

function AdminRoute({ children }: { children: ReactNode }) {
  const { state, loading } = useAuth();
  if (loading) return <p className="route-loading">Loading console...</p>;
  if (!state) return <Navigate to="/login" replace />;
  if (state.role !== "admin") return <Navigate to="/" replace />;
  return <>{children}</>;
}

function HealthIndicator({ api }: { api: ApiClient }) {
  const [health, setHealth] = useState<HealthViewState>("checking");

  useEffect(() => {
    const ctrl = new AbortController();
    const check = () => loadHealth(api, ctrl.signal).then(setHealth).catch(() => undefined);
    check();
    const interval = setInterval(check, 15_000);
    return () => {
      ctrl.abort();
      clearInterval(interval);
    };
  }, [api]);

  const label = health === "healthy" ? "reachable" : health === "checking" ? "checking" : "unavailable";
  return <span className={`health-indicator health-indicator--${health}`}><i />orchestrator: {label}</span>;
}

const navItems = [
  ["/", "Overview"],
  ["/workflows", "Workflows"],
  ["/queue", "Queue"],
  ["/projects", "Projects"],
  ["/runners", "Runners"],
] as const;

function Mark() {
  return <svg width="30" height="30" viewBox="0 0 30 30" aria-hidden="true"><path d="M6 4C10 12 8 20 5 26M15 3C14 12 16 20 15 27M24 4C21 11 23 17 25 22" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"/><circle cx="25" cy="22" r="2.4" /></svg>;
}

function Layout({ children }: { children: ReactNode }) {
  const { state, logout } = useAuth();
  const isAdmin = useIsAdmin();
  const location = useLocation();
  const [menuOpen, setMenuOpen] = useState(false);

  useEffect(() => { setMenuOpen(false); }, [location.pathname]);

  useEffect(() => {
    if (!menuOpen) return;
    const handler = (e: KeyboardEvent) => { if (e.key === "Escape") setMenuOpen(false); };
    document.addEventListener("keydown", handler);
    return () => document.removeEventListener("keydown", handler);
  }, [menuOpen]);

  if (location.pathname === "/login") return <>{children}</>;
  return <>
    <div className="mobile-header">
      <button type="button" className="mobile-menu-toggle" onClick={() => setMenuOpen(!menuOpen)} aria-label="Toggle navigation" aria-expanded={menuOpen}><span className="ham" /></button>
      <Link className="wordmark" to="/"><Mark /><span><b>Moirai</b><small>Console</small></span></Link>
    </div>
    <div className="app-layout">
      <aside className="side">
        <Link className="wordmark" to="/"><Mark /><span><b>Moirai</b><small>Console</small></span></Link>
        <span className="navlab">Control plane</span>
        <nav aria-label="Console navigation">
          {navItems.map(([to, label]) => <NavLink key={to} end={to === "/"} to={to} className="nav-item"><i />{label}</NavLink>)}
          {isAdmin && <NavLink to="/tokens" className="nav-item"><i />Runner tokens</NavLink>}
        </nav>
        <div className="side-foot"><span>{state?.username} · {state?.role}</span><HealthIndicator api={api} /><button className="quiet-button" onClick={() => void logout()}>Sign out</button></div>
      </aside>
      <main className="app-content">{children}</main>
    </div>
    {menuOpen && <div className="mobile-menu-backdrop" onClick={() => setMenuOpen(false)} />}
    <aside className={`mobile-menu${menuOpen ? " mobile-menu--open" : ""}`} hidden={!menuOpen} aria-label="Mobile navigation">
      <Link className="wordmark" to="/"><Mark /><span><b>Moirai</b><small>Console</small></span></Link>
      <span className="navlab">Control plane</span>
      <nav aria-label="Console navigation">
        {navItems.map(([to, label]) => <NavLink key={to} end={to === "/"} to={to} className="nav-item" onClick={() => setMenuOpen(false)}><i />{label}</NavLink>)}
        {isAdmin && <NavLink to="/tokens" className="nav-item" onClick={() => setMenuOpen(false)}><i />Runner tokens</NavLink>}
      </nav>
      <div className="side-foot"><span>{state?.username} · {state?.role}</span><HealthIndicator api={api} /><button className="quiet-button" onClick={() => void logout()}>Sign out</button></div>
    </aside>
  </>;
}

function Pill({ status }: { status: string }) {
  const terminal = status === "completed";
  const bad = status === "blocked" || status === "failed" || status === "cancelled";
  const wait = status.startsWith("waiting_") || status === "pr_created";
  return <span className={`pill ${terminal ? "pill--ok" : bad ? "pill--bad" : wait ? "pill--wait" : "pill--run"}`}><i />{status.replaceAll("_", " ")}</span>;
}

function Dashboard() {
  const [data, setData] = useState<{ projects: Project[]; runners: Runner[]; workflows: Workflow[] } | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ctrl = new AbortController();
    Promise.all([api.listProjects(ctrl.signal), api.listRunners(ctrl.signal), api.listWorkflows(ctrl.signal)])
      .then(([projects, runners, workflows]) => setData({ projects, runners, workflows }))
      .catch((reason: unknown) => { if (!ctrl.signal.aborted) setError(reason instanceof Error ? reason.message : "Could not load overview."); });
    return () => ctrl.abort();
  }, []);

  const active = data?.workflows.filter(({ status }) => !["completed", "blocked", "failed", "cancelled"].includes(status)).length;
  const waiting = data?.workflows.filter(({ status }) => status === "waiting_human").length;
  return <section>
    <div className="view-head"><h1>Overview</h1><span className="crumb">Control plane</span></div>
    <p className="view-sub">Current workload, runner fleet, and workflows needing a human hand.</p>
    {error && <p className="error" role="alert">Could not load overview: {error}</p>}
    <div className="health-strip"><span><i />API-backed console</span><span><i />Workflow state live</span><span className="placeholder">[placeholder — scheduler metrics]</span></div>
    <div className="grid cards-4">
      <div className="card tile"><span>Registered projects</span><strong>{data ? data.projects.length : "…"}</strong><small>API-backed</small></div>
      <div className="card tile"><span>Active workflows</span><strong>{data ? active : "…"}</strong><small>API-backed</small></div>
      <div className="card tile"><span>Online runners</span><strong>{data ? data.runners.filter((runner) => runner.status === "online").length : "…"}</strong><small>API-backed</small></div>
      <div className="card tile"><span>Needs decision</span><strong>{data ? waiting : "…"}</strong><small>API-backed</small></div>
    </div>
    <div className="grid cols-2 section-gap">
      <section className="card"><div className="card-h"><h2>Recent workflows</h2><Link to="/workflows">View all</Link></div><div className="table-wrap"><table><thead><tr><th>Workflow</th><th>Status</th><th>Phase</th></tr></thead><tbody>{data === null ? <tr><td colSpan={3}>Loading workflows...</td></tr> : data.workflows.length === 0 ? <tr><td colSpan={3} className="table-empty">No workflows have started yet.</td></tr> : data.workflows.slice(0, 5).map((workflow) => <tr key={workflow.id}><td><Link className="mono" to={`/workflows/${workflow.id}`}>{workflow.id.slice(0, 12)}</Link></td><td><Pill status={workflow.status} /></td><td>{workflow.phase}</td></tr>)}</tbody></table></div></section>
      <section className="card"><div className="card-h"><h2>Global queue</h2><Link to="/queue">View queue</Link></div><p className="empty-state">[placeholder — eligible issues will appear here]</p></section>
    </div>
  </section>;
}

function App() {
  return <BrowserRouter><AuthProvider api={api}><Layout><Routes>
    <Route path="/login" element={<LoginPage />} />
    <Route path="/" element={<ProtectedRoute><Dashboard /></ProtectedRoute>} />
    <Route path="/projects" element={<ProtectedRoute><ProjectsPage api={api} /></ProtectedRoute>} />
    <Route path="/runners" element={<ProtectedRoute><RunnersPage api={api} /></ProtectedRoute>} />
    <Route path="/tokens" element={<AdminRoute><TokensPage api={api} /></AdminRoute>} />
    <Route path="/workflows" element={<ProtectedRoute><WorkflowsPage api={api} /></ProtectedRoute>} />
    <Route path="/queue" element={<ProtectedRoute><QueuePage api={api} /></ProtectedRoute>} />
    <Route path="/workflows/:workflowId" element={<ProtectedRoute><WorkflowDetailPage api={api} /></ProtectedRoute>} />
  </Routes></Layout></AuthProvider></BrowserRouter>;
}

createRoot(document.getElementById("root")!).render(<ErrorBoundary><App /></ErrorBoundary>);
