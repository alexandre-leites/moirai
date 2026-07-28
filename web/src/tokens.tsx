import { useCallback, useEffect, useState } from "react";
import type { ApiClient, CreatedToken, RunnerToken } from "./api";
import { useControlPlaneEvents } from "./events";

export function TokensPage({ api }: { api: ApiClient }) {
  const [tokens, setTokens] = useState<RunnerToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [created, setCreated] = useState<CreatedToken | null>(null);
  const [labels, setLabels] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState<string | null>(null);
  const load = useCallback(async () => {
    try { setTokens(await api.listTokens()); setError(null); } catch (err) { setError(err instanceof Error ? err.message : "Could not load runner tokens"); } finally { setLoading(false); }
  }, [api]);
  useEffect(() => { void load(); }, [load]);
  useControlPlaneEvents(useCallback(() => { void load(); }, [load]));
  const handleCreate = async () => {
    setPending("create"); setError(null);
    try { setCreated(await api.createToken(labels.split(",").map(label => label.trim()).filter(Boolean))); setLabels(""); await load(); } catch (err) { setError(err instanceof Error ? err.message : "Could not create runner token"); } finally { setPending(null); }
  };
  const handleRevoke = async (id: string) => {
    setPending(id); setError(null);
    try { await api.revokeToken(id); setTokens(current => current.filter(token => token.id !== id)); } catch (err) { setError(err instanceof Error ? err.message : "Could not revoke runner token"); } finally { setPending(null); }
  };
  return <div><h2>Runner Tokens</h2>{error && <p className="error" role="alert">{error}</p>}<section className="section"><h3>Create token</h3><div className="form-inline"><input value={labels} onChange={event => setLabels(event.target.value)} placeholder="Allowed labels (comma-separated)" disabled={pending === "create"} /><button onClick={() => void handleCreate()} disabled={pending === "create"}>{pending === "create" ? "Generating..." : "Generate"}</button></div>{created && <div className="token-display"><p className="warning">This token is shown once. Copy it now.</p><code>{created.token}</code><p>Expires: {new Date(created.expiresAt).toLocaleString()}</p><button onClick={() => setCreated(null)}>Dismiss</button></div>}</section>{loading ? <p aria-live="polite">Loading tokens...</p> : <table><thead><tr><th>ID</th><th>Labels</th><th>Expires</th><th>Used</th><th>Actions</th></tr></thead><tbody>{tokens.length === 0 ? <tr><td colSpan={5}>No tokens</td></tr> : tokens.map(token => <tr key={token.id}><td className="mono">{token.id.slice(0, 8)}</td><td>{token.allowedLabels.join(", ") || "-"}</td><td>{new Date(token.expiresAt).toLocaleDateString()}</td><td>{token.usedAt ? new Date(token.usedAt).toLocaleDateString() : "Unused"}</td><td>{!token.revokedAt && !token.usedAt && <button onClick={() => void handleRevoke(token.id)} disabled={pending === token.id}>{pending === token.id ? "Revoking..." : "Revoke"}</button>}</td></tr>)}</tbody></table>}</div>;
}
