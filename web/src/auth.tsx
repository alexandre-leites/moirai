import { useCallback, useEffect, useState, type ReactNode } from "react";
import type { ApiClient, CurrentUser } from "./api";
import { AuthContext } from "./auth-context";

type AuthState = CurrentUser;

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

  const refresh = useCallback(async () => {
    setState(await api.me());
  }, [api]);

  return (
    <AuthContext.Provider value={{ state, loading, login, logout, refresh }}>
      {children}
    </AuthContext.Provider>
  );
}
