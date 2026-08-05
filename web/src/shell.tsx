// The app shell: sidebar with live counts, the drawer that replaces it at narrow
// widths, per-route document titles, and the routing table (specification.md
// §3.1 and §5.7).
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, NavLink, Navigate, Route, Routes, useLocation } from "react-router";
import type { ApiClient } from "./api";
import { useAuth } from "./auth-context";
import { ConsoleDataProvider } from "./console-data";
import { activeWorkflows, needsAttention, onlineRunners, useConsoleData } from "./console-data-context";
import { AccountPage } from "./account";
import { OverviewPage } from "./overview";
import { ProjectsPage } from "./projects";
import { QueuePage } from "./queue";
import { RunnersPage } from "./runners";
import { WorkflowsPage } from "./workflows";
import { WorkflowDetailPage } from "./workflow-detail";
import { useFocusTrap } from "./ui/focus-trap";
import { resolvedTheme, useTheme } from "./theme";
import { routeTitle } from "./route-title";

function Mark() {
  return (
    <svg width="30" height="30" viewBox="0 0 30 30" aria-hidden="true">
      <path d="M6 4 C 10 12, 8 20, 5 26" fill="none" stroke="var(--thread)" strokeWidth="1.8" strokeLinecap="round" />
      <path d="M15 3 C 14 12, 16 20, 15 27" fill="none" stroke="var(--thread)" strokeWidth="1.8" strokeLinecap="round" opacity="0.75" />
      <path d="M24 4 C 21 11, 23 17, 25 22" fill="none" stroke="var(--thread)" strokeWidth="1.8" strokeLinecap="round" opacity="0.5" />
      <circle cx="25" cy="22" r="2.4" fill="var(--crit)" />
    </svg>
  );
}

function Wordmark() {
  return (
    <Link className="wordmark" to="/">
      <Mark />
      <span>
        <span className="name">Moirai</span>
        <span className="sub">Console</span>
      </span>
    </Link>
  );
}

/** Sidebar counts, live from the shared snapshot (specification.md §3.1). */
function Navigation({ onNavigate }: { onNavigate?: () => void }) {
  const { data } = useConsoleData();
  const active = data ? activeWorkflows(data.workflows) : [];
  const hot = data ? needsAttention(data.workflows).length > 0 : false;

  const items: Array<{ to: string; label: string; count?: string; hot?: boolean }> = [
    { to: "/", label: "Overview" },
    { to: "/queue", label: "Queue", count: data ? String(data.queue.length) : undefined },
    { to: "/workflows", label: "Workflows", count: data ? String(active.length) : undefined, hot },
    { to: "/runners", label: "Runners", count: data ? `${onlineRunners(data.runners).length}/${data.runners.length}` : undefined },
    { to: "/projects", label: "Projects" },
  ];

  return (
    <nav aria-label="Console navigation">
      <div className="navlab">Operate</div>
      {items.map((item) => (
        <NavLink key={item.to} to={item.to} end={item.to === "/"} className="nav-item" onClick={onNavigate}>
          <span className="nav-dot" />
          {item.label}
          {item.count !== undefined && (
            <span className={item.hot ? "count hot" : "count"} aria-live="polite">{item.count}</span>
          )}
        </NavLink>
      ))}
    </nav>
  );
}

function SidebarFooter() {
  const { state, logout } = useAuth();
  const { theme, toggle } = useTheme();
  const { data, error } = useConsoleData();

  // The shared snapshot is the honest reachability signal: it is the same set of
  // calls the console needs in order to render anything at all.
  const reachability = data === null && error === null ? "checking" : error === null ? "reachable" : "unreachable";

  return (
    <div className="foot">
      <div className="who">{state?.username} · {state?.role}</div>
      <div>orchestrator: {reachability}</div>
      <Link to="/account">Account</Link>
      <button type="button" className="quiet-button" onClick={toggle}>Theme: {resolvedTheme(theme)}</button>
      <button type="button" className="quiet-button" onClick={() => void logout()}>Sign out</button>
    </div>
  );
}

function Drawer({ onClose }: { onClose: () => void }) {
  const panel = useRef<HTMLElement | null>(null);
  useFocusTrap(panel, onClose);

  return (
    <>
      <div className="drawer-scrim" onClick={onClose} />
      <aside className="side drawer" aria-label="Console navigation" ref={panel}>
        <Wordmark />
        <Navigation onNavigate={onClose} />
        <SidebarFooter />
      </aside>
    </>
  );
}

export function Shell({ children }: { children: ReactNode }) {
  const location = useLocation();
  const [drawerOpen, setDrawerOpen] = useState(false);

  useEffect(() => { document.title = routeTitle(location.pathname); }, [location.pathname]);
  useEffect(() => { setDrawerOpen(false); }, [location.pathname]);

  return (
    <>
      <div className="topbar">
        <button
          type="button"
          className="hamburger"
          aria-label="Open navigation"
          aria-expanded={drawerOpen}
          onClick={() => setDrawerOpen(true)}
        >
          <span className="ham" />
        </button>
        <Wordmark />
      </div>
      <div className="app">
        <aside className="side">
          <Wordmark />
          <Navigation />
          <SidebarFooter />
        </aside>
        <main className="main">{children}</main>
      </div>
      {drawerOpen && <Drawer onClose={() => setDrawerOpen(false)} />}
    </>
  );
}

export function NotFound() {
  return (
    <div>
      <div className="view-head"><h1>Nothing here</h1></div>
      <p className="view-sub">
        That address does not match a console view. <Link to="/">Back to the overview</Link>.
      </p>
    </div>
  );
}

/** Everything behind a session: the shared poller and the shell live here. */
export function Console({ api }: { api: ApiClient }) {
  const { state, loading } = useAuth();
  if (loading) return <p className="empty">Loading console…</p>;
  if (!state) return <Navigate to="/login" replace />;

  return (
    <ConsoleDataProvider api={api}>
      <Shell>
        <Routes>
          <Route path="/" element={<OverviewPage api={api} />} />
          <Route path="/queue" element={<QueuePage api={api} />} />
          <Route path="/workflows" element={<WorkflowsPage />} />
          <Route path="/workflows/:workflowId" element={<WorkflowDetailPage api={api} />} />
          <Route path="/runners" element={<RunnersPage api={api} />} />
          {/* The registration-token page folded into Runners (specification.md §5.5). */}
          <Route path="/tokens" element={<Navigate to="/runners" replace />} />
          <Route path="/projects" element={<ProjectsPage api={api} />} />
          <Route path="/account" element={<AccountPage api={api} />} />
          <Route path="*" element={<NotFound />} />
        </Routes>
      </Shell>
    </ConsoleDataProvider>
  );
}
