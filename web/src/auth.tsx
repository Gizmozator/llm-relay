import { createContext, useContext, useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import {
  api,
  clearSession,
  getToken,
  loadStoredUser,
  saveSession,
  setUnauthorizedHandler,
} from './api';
import type { User } from './api';

interface AuthContextValue {
  ready: boolean;
  user: User | null;
  login: (username: string, password: string) => Promise<void>;
  logout: () => void;
  /** Replace the stored user object after a /api/me update (e.g. display name). */
  refreshUser: (u: User) => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(() => loadStoredUser());
  const [ready, setReady] = useState(false);

  useEffect(() => {
    setUnauthorizedHandler(() => setUser(null));
    if (!getToken()) {
      setReady(true);
      return;
    }
    // Revalidate the stored token and refresh the role.
    api
      .me()
      .then((u) => {
        saveSession({ token: getToken() ?? '', user: u });
        setUser(u);
      })
      .catch(() => {
        clearSession();
        setUser(null);
      })
      .finally(() => setReady(true));
  }, []);

  const login = async (username: string, password: string) => {
    const s = await api.login(username, password);
    saveSession(s);
    setUser(s.user);
  };

  const logout = () => {
    clearSession();
    setUser(null);
  };

  const refreshUser = (u: User) => {
    const token = getToken();
    if (token) saveSession({ token, user: u });
    setUser(u);
  };

  return (
    <AuthContext.Provider value={{ ready, user, login, logout, refreshUser }}>
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used inside AuthProvider');
  return ctx;
}
