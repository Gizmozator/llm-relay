import { useState } from 'react';
import type { FormEvent } from 'react';
import { api, errMsg } from '../api';
import type { Role } from '../api';
import { fmtDateTime } from '../format';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import { Banner, Empty, Field, Loading, Modal } from '../components/ui';
import { useAuth } from '../auth';

export default function UsersPage() {
  const toast = useToast();
  const { user: me } = useAuth();
  const users = useLoad(() => api.users().then((r) => r.users), []);

  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [role, setRole] = useState<Role>('user');
  const [busy, setBusy] = useState(false);
  const [resetFor, setResetFor] = useState<number | null>(null);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    if (!username.trim() || !password) return;
    setBusy(true);
    try {
      await api.createUser(username.trim(), password, role);
      toast.push('success', `User “${username.trim()}” created.`);
      setUsername('');
      setPassword('');
      setRole('user');
      users.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  const patch = async (id: number, p: { disabled?: boolean; role?: Role }) => {
    try {
      await api.patchUser(id, p);
      users.reload();
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
      users.reload();
    }
  };

  return (
    <div className="page">
      <div className="page-head">
        <h2>Users</h2>
      </div>

      <form className="panel form-row" onSubmit={create}>
        <Field label="Username">
          <input
            className="input"
            value={username}
            autoComplete="off"
            onChange={(e) => setUsername(e.target.value)}
          />
        </Field>
        <Field label="Password">
          <input
            className="input"
            type="password"
            value={password}
            autoComplete="new-password"
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>
        <Field label="Role">
          <select className="input" value={role} onChange={(e) => setRole(e.target.value as Role)}>
            <option value="user">user</option>
            <option value="admin">admin</option>
          </select>
        </Field>
        <button className="btn primary" type="submit" disabled={busy || !username.trim() || !password}>
          {busy ? 'Creating…' : 'Create user'}
        </button>
      </form>

      <section className="panel">
        {users.loading && !users.data ? (
          <Loading />
        ) : users.error ? (
          <Banner>{users.error}</Banner>
        ) : !users.data || users.data.length === 0 ? (
          <Empty label="No users." />
        ) : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>ID</th>
                  <th>Username</th>
                  <th>Role</th>
                  <th>Status</th>
                  <th>Created</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {users.data.map((u) => {
                  const isSelf = me?.id === u.id;
                  return (
                    <tr key={u.id}>
                      <td className="num">{u.id}</td>
                      <td>
                        {u.username}
                        {isSelf && <span className="tag">you</span>}
                      </td>
                      <td>
                        <select
                          className="input input-inline"
                          value={u.role}
                          disabled={isSelf}
                          onChange={(e) => patch(u.id, { role: e.target.value as Role })}
                          aria-label={`Role of ${u.username}`}
                        >
                          <option value="user">user</option>
                          <option value="admin">admin</option>
                        </select>
                      </td>
                      <td>
                        {u.disabled ? (
                          <span className="badge err">disabled</span>
                        ) : (
                          <span className="badge ok">active</span>
                        )}
                      </td>
                      <td className="nowrap">{fmtDateTime(u.created_at)}</td>
                      <td className="actions">
                        <button className="btn small" onClick={() => setResetFor(u.id)}>
                          Reset password
                        </button>
                        {!isSelf && (
                          <button className="btn small" onClick={() => patch(u.id, { disabled: !u.disabled })}>
                            {u.disabled ? 'Enable' : 'Disable'}
                          </button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {resetFor !== null && (
        <ResetPasswordModal
          userId={resetFor}
          onClose={() => setResetFor(null)}
          onDone={() => {
            toast.push('success', 'Password reset.');
            setResetFor(null);
          }}
        />
      )}
    </div>
  );
}

function ResetPasswordModal({
  userId,
  onClose,
  onDone,
}: {
  userId: number;
  onClose: () => void;
  onDone: () => void;
}) {
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!password) return;
    setBusy(true);
    setError(null);
    try {
      await api.patchUser(userId, { password });
      onDone();
    } catch (err: unknown) {
      setError(errMsg(err));
      setBusy(false);
    }
  };

  return (
    <Modal title="Reset password" onClose={onClose}>
      <form onSubmit={submit} className="form-stack">
        {error && <Banner>{error}</Banner>}
        <Field label="New password">
          <input
            className="input"
            type="password"
            value={password}
            autoFocus
            autoComplete="new-password"
            onChange={(e) => setPassword(e.target.value)}
          />
        </Field>
        <div className="modal-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn primary" disabled={busy || !password}>
            {busy ? 'Saving…' : 'Set password'}
          </button>
        </div>
      </form>
    </Modal>
  );
}
