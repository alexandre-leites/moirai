import { useEffect, useState } from "react";
import type { ApiClient, CreatedToken, RunnerToken } from "./api";

export function TokensPage({ api }: { api: ApiClient }) {
  const [tokens, setTokens] = useState<RunnerToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [created, setCreated] = useState<CreatedToken | null>(null);
  const [labels, setLabels] = useState("");
  const [error, setError] = useState("");
  const [creating, setCreating] = useState(false);
  const [revoking, setRevoking] = useState<Set<string>>(new Set());

  const load = () => {
    const ctrl = new AbortController();
    setLoading(true);
    api.listTokens(ctrl.signal).then(setTokens).catch(() => undefined).finally(() => setLoading(false));
    return () => ctrl.abort();
  };

  useEffect(() => load(), [api]);

  const handleCreate = async () => {
    setError("");
    setCreating(true);
    try {
      const result = await api.createToken(labels.split(",").map((l) => l.trim()).filter(Boolean));
      setCreated(result);
      setLabels("");
      load();
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Token creation failed");
    } finally {
      setCreating(false);
    }
  };

  const handleRevoke = async (id: string) => {
    setError("");
    setRevoking((prev) => new Set(prev).add(id));
    try {
      await api.revokeToken(id);
      setTokens(tokens.filter((t) => t.id !== id));
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : "Token revocation failed");
    } finally {
      setRevoking((prev) => {
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
    }
  };

  return (
    <div>
      <div className="view-head"><h1>Runner tokens</h1></div>
      <p className="view-sub">One-time credentials for registering trusted runners.</p>
      <section className="section">
        <h3>Create token</h3>
        <div className="form-inline">
          <input
            value={labels}
            onChange={(e) => setLabels(e.target.value)}
            placeholder="Allowed labels (comma-separated)"
          />
          <button onClick={handleCreate} disabled={creating}>
            {creating ? "Generating..." : "Generate"}
          </button>
        </div>
        {error && <p className="error" role="alert">{error}</p>}
        {created && (
          <div className="token-display">
            <p className="warning">This token is shown once. Copy it now.</p>
            <code>{created.token}</code>
            <p>Expires: {new Date(created.expiresAt).toLocaleString()}</p>
            <button onClick={() => setCreated(null)}>Dismiss</button>
          </div>
        )}
      </section>
      {loading ? <p>Loading tokens...</p> : tokens.length === 0 ? <p className="empty-state">No active runner tokens. Generate one when a runner needs to register.</p> : (
        <table>
          <thead><tr><th>ID</th><th>Labels</th><th>Expires</th><th>Used</th><th>Actions</th></tr></thead>
          <tbody>
            {tokens.map((t) => (
              <tr key={t.id}>
                <td className="mono">{t.id.slice(0, 8)}</td>
                <td>{t.allowedLabels.join(", ") || "-"}</td>
                <td>{new Date(t.expiresAt).toLocaleDateString()}</td>
                <td>{t.usedAt ? new Date(t.usedAt).toLocaleDateString() : "Unused"}</td>
                <td>
                  {!t.revokedAt && !t.usedAt && (
                    <button onClick={() => handleRevoke(t.id)} disabled={revoking.has(t.id)}>
                      {revoking.has(t.id) ? "Revoking..." : "Revoke"}
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
