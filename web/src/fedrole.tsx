import { createContext, useCallback, useContext, useEffect, useState } from 'react';
import type { ReactNode } from 'react';
import { useLocation } from 'react-router-dom';
import { api, errMsg } from './api';
import type { FedRole } from './api';
import { useAuth } from './auth';

interface FedRoleContextValue {
  /** Persisted instance role; null until the first load lands (or if it failed). */
  role: FedRole | null;
  error: string | null;
  /** Record the role the server just persisted, from a settings save. */
  saved: (role: FedRole) => void;
}

const FedRoleContext = createContext<FedRoleContextValue | null>(null);

/**
 * The instance's federation role as shared state. The role decides the menu
 * structure (the sidebar shows the setup guide only for a hub) and the panels
 * on the Federation page, so it belongs above the router outlet: an admin who
 * saves a new role must see the menu change at once, not at the next
 * navigation or reload. Readers get `role`; the page that writes the role on
 * the server calls `saved` with what the server echoed back.
 */
export function FedRoleProvider({ children }: { children: ReactNode }) {
  const { ready, user } = useAuth();
  const location = useLocation();
  const [role, setRole] = useState<FedRole | null>(null);
  const [error, setError] = useState<string | null>(null);

  // The endpoint is session-auth, so the load waits for a session (a fetch on
  // the login page would only 401). Keyed on the path as the per-page load it
  // replaces was, so a role changed out of band still lands on the next click;
  // `saved` covers the in-app change, so nothing needs polling.
  useEffect(() => {
    if (!ready || !user) return;
    let cancelled = false;
    api
      .fedRole()
      .then((r) => {
        if (!cancelled) {
          setRole(r.role);
          setError(null);
        }
      })
      .catch((e: unknown) => {
        // The sidebar has no banner surface worth dirtying; a failure just
        // hides the hub-only entry, but it must not fail silent.
        if (!cancelled) setError(errMsg(e));
      });
    return () => {
      cancelled = true;
    };
  }, [ready, user, location.pathname]);

  const saved = useCallback((next: FedRole) => {
    setRole(next);
    setError(null);
  }, []);

  return (
    <FedRoleContext.Provider value={{ role, error, saved }}>
      {children}
    </FedRoleContext.Provider>
  );
}

export function useFedRole(): FedRoleContextValue {
  const ctx = useContext(FedRoleContext);
  if (!ctx) throw new Error('useFedRole must be used inside FedRoleProvider');
  return ctx;
}
