import { useState } from 'react';
import { NavLink, Navigate, Outlet } from 'react-router-dom';
import { useAuth } from './auth';
import { brandName } from './brand';
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
            Usage
          </NavLink>
          <NavLink to="/keys" className={navClass}>
            My keys
          </NavLink>
          <NavLink to="/models" className={navClass}>
            Models
          </NavLink>
          {isAdmin && <div className="nav-group">Admin</div>}
          {isAdmin && (
            <>
              <NavLink to="/admin/backends" className={navClass}>
                Backends
              </NavLink>
              <NavLink to="/admin/models" className={navClass}>
                Models
              </NavLink>
              <NavLink to="/admin/users" className={navClass}>
                Users
              </NavLink>
              <NavLink to="/admin/usage" className={navClass}>
                All usage
              </NavLink>
              <NavLink to="/admin/settings" className={navClass}>
                Settings
              </NavLink>
            </>
          )}
        </nav>
      </aside>
      <div className="main">
        <header className="topbar">
          <div className="topbar-user">
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
        <div className="content">
          <Outlet />
        </div>
      </div>
      {showAccount && <AccountModal onClose={() => setShowAccount(false)} />}
    </div>
  );
}
