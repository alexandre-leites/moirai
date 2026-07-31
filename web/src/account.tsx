import { useEffect, useRef, useState, type FormEvent } from "react";
import type { ApiClient } from "./api";
import { useAuth } from "./auth";

export function AccountPage({ api }: { api: ApiClient }) {
  const { state, refresh } = useAuth();
  const [displayName, setDisplayName] = useState(state?.displayName ?? "");
  const [newEmail, setNewEmail] = useState(state?.email ?? "");
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const hydrated = useRef(state !== null);

  useEffect(() => {
    if (hydrated.current || state === null) return;
    hydrated.current = true;
    setDisplayName(state.displayName ?? "");
    setNewEmail(state.email ?? "");
  }, [state]);

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    setSaved(false);
    if (newPassword && newPassword !== confirmPassword) {
      setError("New passwords do not match");
      return;
    }
    setSaving(true);
    try {
      await api.updateAccount({
        currentPassword: currentPassword || undefined,
        newPassword: newPassword || undefined,
        newEmail: newEmail.trim() || undefined,
        displayName: displayName.trim() || undefined,
      });
      setCurrentPassword("");
      setNewPassword("");
      setConfirmPassword("");
      setSaved(true);
      await refresh();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "Could not update the account.");
    } finally {
      setSaving(false);
    }
  };

  return (
    <section>
      <div className="view-head"><h1>Account</h1><span className="crumb">Control plane</span></div>
      <p className="view-sub">Update your display name, email, or password.</p>
      <form onSubmit={handleSubmit} className="account-form">
        {error && <p className="error" role="alert">{error}</p>}
        {saved && <p className="notice" role="status">Account updated.</p>}
        <label>
          Username
          <input type="text" value={state?.username ?? ""} disabled />
        </label>
        <label>
          Display name
          <input type="text" value={displayName} onChange={(e) => setDisplayName(e.target.value)} autoComplete="name" disabled={saving} />
        </label>
        <label>
          Email
          <input type="email" value={newEmail} onChange={(e) => setNewEmail(e.target.value)} autoComplete="email" disabled={saving} />
        </label>
        <div className="form-divider">Change password</div>
        <label>
          Current password
          <input type="password" value={currentPassword} onChange={(e) => setCurrentPassword(e.target.value)} autoComplete="current-password" disabled={saving} />
        </label>
        <label>
          New password
          <input type="password" value={newPassword} onChange={(e) => setNewPassword(e.target.value)} autoComplete="new-password" disabled={saving} />
        </label>
        <label>
          Confirm new password
          <input type="password" value={confirmPassword} onChange={(e) => setConfirmPassword(e.target.value)} autoComplete="new-password" disabled={saving} />
        </label>
        <button type="submit" disabled={saving} aria-busy={saving}>
          {saving && <span className="spinner" aria-hidden="true" />}
          {saving ? "Saving..." : "Save changes"}
        </button>
      </form>
    </section>
  );
}
