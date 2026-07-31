import { useState, type FormEvent } from "react";
import { Navigate } from "react-router-dom";
import { useAuth } from "./auth";

export function LoginPage() {
  const { login, state } = useAuth();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    if (!username.trim() || !password) {
      setError("Username and password are required");
      return;
    }
    setLoading(true);
    try {
      await login(username, password);
    } catch {
      setError("Login failed. Check your credentials.");
    } finally {
      setLoading(false);
    }
  };

  if (state) return <Navigate to="/" replace />;

  return (
    <main className="login-page">
      <h1>Moirai</h1>
      <form onSubmit={handleSubmit} className="login-form">
        <h2>Sign in</h2>
        {error && <p className="error" role="alert">{error}</p>}
        <label>
          Username
          <input
            type="text"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
            disabled={loading}
          />
        </label>
        <label>
          Password
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            disabled={loading}
          />
        </label>
        <button type="submit" disabled={loading} aria-busy={loading}>
          {loading && <span className="spinner" aria-hidden="true" />}
          {loading ? "Signing in..." : "Sign in"}
        </button>
      </form>
    </main>
  );
}
