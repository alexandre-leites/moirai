import { createContext, useContext, useCallback, useEffect, useState, type ReactNode } from "react";
import type { ApiClient, CurrentUser } from "./api";

type AuthState = CurrentUser;

type AuthContextValue = {
  state: AuthState | null;
  // True until the initial GET /api/v1/auth/me resolves. Consumers (ProtectedRoute)
  // must wait for this before deciding whether to redirect to /login — otherwise a
  // page refresh with a perfectly valid session cookie briefly reads as logged out.
  loading: boolean;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
};

const AuthContext = createContext<AuthContextValue>({
  state: null,
  loading: true,
  login: async () => undefined,
  logout: async () => undefined,
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

export function AuthProvider({ api, children }: { api: ApiClient; children: ReactNode }) {
  const [state, setState] = useState<AuthState | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    api.setUnauthorizedHandler(() => setState(null));
    return () => api.setUnauthorizedHandler(null);
  }, [api]);

  useEffect(() => {
    let cancelled = false;
    api
      .me()
      .then((user) => {
        if (!cancelled) setState(user);
      })
      .catch(() => {
        if (!cancelled) setState(null);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [api]);

  const login = useCallback(async (username: string, password: string) => {
    await api.login(username, password);
    const user = await api.me();
    setState(user);
  }, [api]);

  const logout = useCallback(async () => {
    await api.logout();
    setState(null);
  }, [api]);

  return (
    <AuthContext.Provider value={{ state, loading, login, logout }}>
      {children}
    </AuthContext.Provider>
  );
}
