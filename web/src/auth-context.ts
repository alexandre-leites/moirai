// The auth context and the hooks that read it, split out from auth.tsx so
// that file can export only the AuthProvider component (react-refresh/
// only-export-components: fast refresh needs a module to export components
// only, and useAuth/useUserId/useIsAdmin are plain hooks, not components).
import { createContext, useContext } from "react";
import type { CurrentUser } from "./api";

type AuthState = CurrentUser;

export type AuthContextValue = {
  state: AuthState | null;
  // True until the initial GET /api/v1/auth/me resolves. Consumers (ProtectedRoute)
  // must wait for this before deciding whether to redirect to /login — otherwise a
  // page refresh with a perfectly valid session cookie briefly reads as logged out.
  loading: boolean;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  // Re-fetches the current user after an account update so display name/email
  // changes reflect immediately in the UI.
  refresh: () => Promise<void>;
};

export const AuthContext = createContext<AuthContextValue>({
  state: null,
  loading: true,
  login: async () => undefined,
  logout: async () => undefined,
  refresh: async () => undefined,
});

export function useAuth(): AuthContextValue {
  return useContext(AuthContext);
}

export function useUserId(): string | null {
  return useAuth().state?.userId ?? null;
}

export function useIsAdmin(): boolean {
  return useAuth().state?.role === "admin";
}
