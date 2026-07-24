import { useEffect, useState } from "react";
import type { ApiClient, CreatedToken, RunnerToken } from "./api";

export function TokensPage({ api }: { api: ApiClient }) {
  const [tokens, setTokens] = useState<RunnerToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [created, setCreated] = useState<CreatedToken | null>(null);
  const [labels, setLabels] = useState("");

  const load = () => {
    const ctrl = new AbortController();
    setLoading(true);
    api.listTokens(ctrl.signal).then(setTokens).catch(() => undefined).finally(() => setLoading(false));
    return () => ctrl.abort();
  };

  useEffect(() => load(), [api]);

  const handleCreate = async () => {
    try {
      const result = await api.createToken(labels.split(",").map((l) => l.trim()).filter(Boolean));
      setCreated(result);
      setLabels("");
      load();
    } catch {
      // silently fail
    }
  };

  const handleRevoke = async (id: string) => {
    try {
      await api.revokeToken(id);
      setTokens(tokens.filter((t) => t.id !== id));
    } catch {
      // silently fail
    }
  };

  return (
    <div>
      <h2>Runner Tokens</h2>
      <section className="section">
        <h3>Create token</h3>
        <div className="form-inline">
          <input
            value={labels}
            onChange={(e) => setLabels(e.target.value)}
            placeholder="Allowed labels (comma-separated)"
          />
          <button onClick={handleCreate}>Generate</button>
        </div>
        {created && (
          <div className="token-display">
            <p className="warning">This token is shown once. Copy it now.</p>
            <code>{created.token}</code>
            <p>Expires: {new Date(created.expiresAt).toLocaleString()}</p>
            <button onClick={() => setCreated(null)}>Dismiss</button>
          </div>
        )}
      </section>
      {loading ? <p>Loading tokens...</p> : (
        <table>
          <thead><tr><th>ID</th><th>Labels</th><th>Expires</th><th>Used</th><th>Actions</th></tr></thead>
          <tbody>
            {tokens.length === 0 ? (
              <tr><td colSpan={5}>No tokens</td></tr>
            ) : tokens.map((t) => (
              <tr key={t.id}>
                <td className="mono">{t.id.slice(0, 8)}</td>
                <td>{t.allowedLabels.join(", ") || "-"}</td>
                <td>{new Date(t.expiresAt).toLocaleDateString()}</td>
                <td>{t.usedAt ? new Date(t.usedAt).toLocaleDateString() : "Unused"}</td>
                <td>
                  {!t.revokedAt && !t.usedAt && (
                    <button onClick={() => handleRevoke(t.id)}>Revoke</button>
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
