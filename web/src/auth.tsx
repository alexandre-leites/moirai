import { createContext, useContext, useCallback, useState, type ReactNode } from "react";
import type { ApiClient } from "./api";

type AuthState = {
  userId: string;
};

type AuthContextValue = {
  state: AuthState | null;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
};

const AuthContext = createContext<AuthContextValue>({
  state: null,
  login: async () => undefined,
  logout: async () => undefined,
});

export function useAuth(): AuthContextValue {
  return useContext(AuthContext);
}

export function useUserId(): string | null {
  return useAuth().state?.userId ?? null;
}

export function AuthProvider({ api, children }: { api: ApiClient; children: ReactNode }) {
  const [state, setState] = useState<AuthState | null>(null);

  const login = useCallback(async (username: string, password: string) => {
    const result = await api.login(username, password);
    setState({ userId: result.userId });
  }, [api]);

  const logout = useCallback(async () => {
    await api.logout();
    setState(null);
  }, [api]);

  return (
    <AuthContext.Provider value={{ state, login, logout }}>
      {children}
    </AuthContext.Provider>
  );
}
