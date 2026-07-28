import { Component, useEffect, useState, type ErrorInfo, type ReactNode } from "react";
import { BrowserRouter, Routes, Route, Navigate, Link } from "react-router-dom";
import { createRoot } from "react-dom/client";
import { createApiClient } from "./api";
import type { ApiClient } from "./api";
import { AuthProvider, useAuth, useIsAdmin } from "./auth";
import { loadHealth } from "./health";
import type { HealthViewState } from "./health";
import { LoginPage } from "./login";
import { ProjectsPage } from "./projects";
import { TokensPage } from "./tokens";
import { WorkflowsPage } from "./workflows";
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
      return (
        <div className="error-boundary">
          <h1>Something went wrong</h1>
          <p>{this.state.error.message}</p>
          <button onClick={() => this.setState({ error: null })}>Try again</button>
        </div>
      );
    }
    return this.props.children;
  }
}

function ProtectedRoute({ children }: { children: React.ReactNode }) {
  const { state, loading } = useAuth();
  if (loading) return <p>Loading...</p>;
  if (!state) return <Navigate to="/login" replace />;
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

  const label = health === "healthy" ? "Healthy" : health === "checking" ? "Checking..." : "Unavailable";
  return (
    <span className={`health-indicator health-indicator--${health}`} title="Orchestrator connectivity">
      {label}
    </span>
  );
}

function AdminRoute({ children }: { children: React.ReactNode }) {
  const { state, loading } = useAuth();
  if (loading) return <p>Loading...</p>;
  if (!state) return <Navigate to="/login" replace />;
  if (state.role !== "admin") return <Navigate to="/" replace />;
  return <>{children}</>;
}
function Layout({ children }: { children: React.ReactNode }) {
  const { state, logout } = useAuth();
  const isAdmin = useIsAdmin();
  return (
    <div className="app-layout">
      <header>
        <h1><Link to="/">Moirai</Link></h1>
        {state && (
          <nav>
            <Link to="/projects">Projects</Link>
            {isAdmin && <Link to="/tokens">Tokens</Link>}
            <Link to="/workflows">Workflows</Link>
            <HealthIndicator api={api} />
            <button className="logout-btn" onClick={logout}>Logout</button>
          </nav>
        )}
      </header>
      <main className="app-content">{children}</main>
    </div>
  );
}

function Dashboard() {
  const isAdmin = useIsAdmin();
  return (
    <div>
      <h2>Dashboard</h2>
      <ul className="nav-list">
        <li><Link to="/projects">Projects</Link></li>
        {isAdmin && <li><Link to="/tokens">Runner tokens</Link></li>}
        <li><Link to="/workflows">Workflows</Link></li>
      </ul>
    </div>
  );
}

function App() {
  return (
    <BrowserRouter>
      <AuthProvider api={api}>
        <Layout>
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route path="/" element={<ProtectedRoute><Dashboard /></ProtectedRoute>} />
            <Route path="/projects" element={<ProtectedRoute><ProjectsPage api={api} /></ProtectedRoute>} />
            <Route path="/tokens" element={<AdminRoute><TokensPage api={api} /></AdminRoute>} />
            <Route path="/workflows" element={<ProtectedRoute><WorkflowsPage api={api} /></ProtectedRoute>} />
          </Routes>
        </Layout>
      </AuthProvider>
    </BrowserRouter>
  );
}

createRoot(document.getElementById("root")!).render(
  <ErrorBoundary>
    <App />
  </ErrorBoundary>
);
