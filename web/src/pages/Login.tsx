import { useState } from 'react';
import type { FormEvent } from 'react';
import { Navigate } from 'react-router-dom';
import { useAuth } from '../auth';
import { brandName } from '../brand';
import { errMsg } from '../api';
import { Banner, Field } from '../components/ui';

export default function LoginPage() {
  const { ready, user, login } = useAuth();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Wait until the stored token is revalidated, otherwise a hard load with a
  // stale token flashes /login → / → /login.
  if (ready && user) return <Navigate to="/" replace />;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(username.trim(), password);
    } catch (err: unknown) {
      setError(errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login-wrap">
      <form className="login-card" onSubmit={submit}>
        <h1 className="login-brand">
          <span className="brand-accent">&lt;&nbsp;</span>
          {brandName()}
          <span className="brand-accent">&nbsp;/&gt;</span>
        </h1>
        {error && <Banner>{error}</Banner>}
        <Field label="Username">
          <input
            className="input"
            value={username}
            autoComplete="username"
            autoFocus
            disabled={!ready}
            onChange={(e) => setUsername(e.target.value)}
          />
        </Field>
        <Field label="Password">
          <input
            className="input"
            type="password"
            value={password}
            autoComplete="current-password"
            disabled={!ready}
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>
        {/* !ready: the stored token is still being revalidated — submitting
            (or typing) into a session that is about to be confirmed or
            dropped is a race the operator should not be able to hit. */}
        <button className="btn primary login-btn" type="submit" disabled={busy || !ready}>
          {busy ? 'Signing in…' : ready ? 'Sign in' : 'Checking…'}
        </button>
      </form>
    </div>
  );
}
