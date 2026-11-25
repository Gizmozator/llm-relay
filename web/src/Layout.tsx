import { useEffect, useState } from 'react';
import { NavLink, Navigate, Outlet } from 'react-router-dom';
import { useAuth } from './auth';
import { api, errMsg } from './api';
import type { VersionInfo } from './api';
import { brandName } from './brand';
import { useFedRole } from './fedrole';
import { Loading } from './components/ui';
import AccountModal from './components/AccountModal';
import type { ReactNode } from 'react';

export function RequireAuth({ children }: { children: ReactNode }) {
  const { ready, user } = useAuth();
  if (!ready) return <Loading label="Checking session…" />;
  if (!user) return <Navigate to="/login" replace />;
  return <>{children}</>;
}

export function RequireAdmin({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  if (!user || user.role !== 'admin') return <Navigate to="/" replace />;
  return <>{children}</>;
}

function navClass({ isActive }: { isActive: boolean }): string {
  return isActive ? 'active' : '';
}

export default function Layout() {
  const { user, logout } = useAuth();
  const [showAccount, setShowAccount] = useState(false);
  // Build version (plan 2026-11-25-version-awareness): the topbar chip is
  // informational for everyone; the red banner fires when a CONNECTED
  // instance runs newer than this server — this relay is the old one — and
  // an admin has not muted it (version_warn). Loaded once per session; the
  // fleet does not change often enough to poll.
  const [ver, setVer] = useState<VersionInfo | null>(null);
  useEffect(() => {
    let cancelled = false;
    api
      .version()
      .then((v) => {
        if (!cancelled) setVer(v);
      })
      .catch((e: unknown) => console.warn('version unavailable:', errMsg(e)));
    return () => {
      cancelled = true;
    };
  }, []);
  // The setup guide only makes sense while this instance is a hub, so the nav
  // entry follows the shared federation role — the same state the Federation
  // page updates when an admin saves a new one, which is what makes the menu
  // change without a reload. A failure just hides the entry — the sidebar has
  // no banner surface worth dirtying, but it must not fail silent.
  const fed = useFedRole();
  useEffect(() => {
    if (fed.error) console.warn('federation role unavailable:', fed.error);
  }, [fed.error]);
  if (!user) return <Navigate to="/login" replace />;
  const isAdmin = user.role === 'admin';

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">
          <span className="brand-accent">&lt;&nbsp;</span>
          {brandName()}
          <span className="brand-accent">&nbsp;/&gt;</span>
        </div>
        <nav>
          <NavLink to="/" end className={navClass}>
            Dashboard
          </NavLink>
          {/* Models sits right after Dashboard: it is the page users visit
              most (what can I run, what does it cost). */}
          <NavLink to="/models" className={navClass}>
            Models
          </NavLink>
          <NavLink to="/usage" className={navClass}>
            My usage
          </NavLink>
          {(fed.role === 'hub' || fed.role === 'spoke') && (
            <NavLink to="/fairness" className={navClass}>
              My fairness
            </NavLink>
          )}
          <NavLink to="/keys" className={navClass}>
            My keys
          </NavLink>
          <NavLink to="/agents" className={navClass}>
            Agent API
          </NavLink>
          {fed.role === 'hub' && (
            <NavLink to="/setup" className={navClass}>
              Connect server
            </NavLink>
          )}
          {isAdmin && <div className="nav-group">Admin</div>}
          {isAdmin && (
            <>
              <NavLink to="/admin/backends" className={navClass}>
                Backends
              </NavLink>
              <NavLink to="/admin/models" className={navClass}>
                Model catalog
              </NavLink>
              <NavLink to="/admin/users" className={navClass}>
                Users
              </NavLink>
              <NavLink to="/admin/usage" className={navClass}>
                All usage
              </NavLink>
              <NavLink to="/admin/federation" className={navClass}>
                Federation
              </NavLink>
              <NavLink to="/admin/settings" className={navClass}>
                Settings
              </NavLink>
              <NavLink to="/admin/audit" className={navClass}>
                Audit log
              </NavLink>
            </>
          )}
        </nav>
      </aside>
      <div className="main">
        <header className="topbar">
          <div className="topbar-user">
            {ver && (
              <span className="version-chip" title="This relay's build version">
                {ver.version}
              </span>
            )}
            <span className="username">{user.display_name || user.username}</span>
            <span className="role-badge">{user.role}</span>
            <button className="btn small" onClick={() => setShowAccount(true)}>
              Account
            </button>
            <button className="btn small" onClick={logout}>
              Log out
            </button>
          </div>
        </header>
        {ver && ver.outdated && ver.warn_enabled && (
          <div className="banner banner-error version-banner" role="alert">
            <strong>This relay is out of date.</strong> {ver.version} is older than{' '}
            {ver.newer_than.map((w) => `${w.name} (${w.version})`).join(', ')} —{' '}
            {ver.newer_than.length === 1 ? 'it' : 'they'} {ver.newer_than.length === 1 ? 'runs' : 'run'} a
            newer version. Update this server.
          </div>
        )}
        <div className="content">
          <Outlet />
        </div>
      </div>
      {showAccount && <AccountModal onClose={() => setShowAccount(false)} />}
    </div>
  );
}
