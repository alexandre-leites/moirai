import { BrowserRouter, Routes, Route, Navigate, Link } from "react-router-dom";
import { createRoot } from "react-dom/client";
import { createApiClient } from "./api";
import { AuthProvider, useAuth } from "./auth";
import { LoginPage } from "./login";
import { ProjectsPage } from "./projects";
import { TokensPage } from "./tokens";
import { WorkflowsPage } from "./workflows";
import "./styles.css";

const api = createApiClient();

function ProtectedRoute({ children }: { children: React.ReactNode }) {
  const { state } = useAuth();
  if (!state) return <Navigate to="/login" replace />;
  return <>{children}</>;
}

function Layout({ children }: { children: React.ReactNode }) {
  const { state, logout } = useAuth();
  return (
    <div className="app-layout">
      <header>
        <h1><Link to="/">Moirai</Link></h1>
        {state && (
          <nav>
            <Link to="/projects">Projects</Link>
            <Link to="/tokens">Tokens</Link>
            <Link to="/workflows">Workflows</Link>
            <button className="logout-btn" onClick={logout}>Logout</button>
          </nav>
        )}
      </header>
      <main className="app-content">{children}</main>
    </div>
  );
}

function Dashboard() {
  return (
    <div>
      <h2>Dashboard</h2>
      <ul className="nav-list">
        <li><Link to="/projects">Projects</Link></li>
        <li><Link to="/tokens">Runner tokens</Link></li>
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
            <Route path="/tokens" element={<ProtectedRoute><TokensPage api={api} /></ProtectedRoute>} />
            <Route path="/workflows" element={<ProtectedRoute><WorkflowsPage api={api} /></ProtectedRoute>} />
          </Routes>
        </Layout>
      </AuthProvider>
    </BrowserRouter>
  );
}

createRoot(document.getElementById("root")!).render(<App />);
