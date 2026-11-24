import { useEffect, useState } from 'react';
import type { FormEvent } from 'react';
import { api, errMsg, MIN_PASSWORD_LEN, passwordTooShort } from '../api';
import type { Role, User } from '../api';
import { fmtDateTime } from '../format';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import { Banner, EditableNumber, EditableText, Empty, Field, Loading, Modal } from '../components/ui';
import { useAuth } from '../auth';

export default function UsersPage() {
  const toast = useToast();
  const { user: me } = useAuth();
  const users = useLoad(() => api.users().then((r) => r.users), []);

  const [username, setUsername] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [password, setPassword] = useState('');
  const [role, setRole] = useState<Role>('user');
  const [busy, setBusy] = useState(false);
  const [resetFor, setResetFor] = useState<number | null>(null);
  // Row actions in flight (per user+action): a second click while a
  // patch round-trip is running would double-submit.
  const [pending, setPending] = useState<Record<string, boolean>>({});

  const create = async (e: FormEvent) => {
    e.preventDefault();
    if (!username.trim() || passwordTooShort(password)) return;
    setBusy(true);
    try {
      await api.createUser(username.trim(), password, role, displayName.trim() || undefined);
      toast.push('success', `User “${username.trim()}” created.`);
      setUsername('');
      setDisplayName('');
      setPassword('');
      setRole('user');
      users.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  const patch = async (
    id: number,
    p: { disabled?: boolean; role?: Role; display_name?: string; price_multiplier?: number },
  ): Promise<boolean> => {
    try {
      await api.patchUser(id, p);
      // Reload only on success: the revert contract already restores the
      // input on failure, and a failed refetch would replace the whole
      // table (users.error) in the middle of someone's edit.
      users.reload();
      return true;
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
      return false;
    }
  };

  const toggleDisabled = async (u: User) => {
    const tag = `dis-${u.id}`;
    if (pending[tag]) return;
    setPending((p) => ({ ...p, [tag]: true }));
    try {
      await patch(u.id, { disabled: !u.disabled });
    } finally {
      setPending((p) => {
        const { [tag]: _drop, ...rest } = p;
        return rest;
      });
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
        <Field label="Display name (optional)">
          <input
            className="input"
            value={displayName}
            maxLength={64}
            onChange={(e) => setDisplayName(e.target.value)}
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
        {password !== '' && passwordTooShort(password) && (
          <p className="dim">Password must be at least {MIN_PASSWORD_LEN} characters.</p>
        )}
        <Field label="Role">
          <select className="input" value={role} onChange={(e) => setRole(e.target.value as Role)}>
            <option value="user">user</option>
            <option value="admin">admin</option>
          </select>
        </Field>
        <button
          className="btn primary"
          type="submit"
          disabled={busy || !username.trim() || passwordTooShort(password)}
        >
          {busy ? 'Creating…' : 'Create user'}
        </button>
      </form>

      <section className="panel">
        {users.loading && !users.data ? (
          <Loading />
        ) : !users.data || users.data.length === 0 ? (
          users.error ? (
            <Banner>{users.error}</Banner>
          ) : (
            <Empty label="No users." />
          )
        ) : (
          <>
            {/* A failed background refetch must NOT replace the table: the
                last good rows stay on screen (useLoad retains data) so
                inline editors mid-edit are not unmounted — the per-cell
                adopt rule already folds real server changes in by id. */}
            {users.error && (
              <Banner>Refresh failed — showing the last loaded values. {users.error}</Banner>
            )}
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>ID</th>
                    <th>Username</th>
                    <th>Name</th>
                    <th>Role</th>
                    <th className="num">
                      Multiplier
                      <div className="th-sub">price ×</div>
                    </th>
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
                          <EditableText
                            value={u.display_name ?? ''}
                            allowEmpty
                            placeholder="—"
                            ariaLabel={`Display name of ${u.username}`}
                            onCommit={(v) => patch(u.id, { display_name: v })}
                          />
                        </td>
                        <td>
                          <RoleSelect
                            user={u}
                            disabled={isSelf}
                            onPatch={(next) => patch(u.id, { role: next })}
                          />
                        </td>
                        <td className="num">
                          <EditableNumber
                            display={String(u.price_multiplier ?? 1)}
                            max={1000}
                            ariaLabel={`Price multiplier of ${u.username}`}
                            onCommit={(n) => patch(u.id, { price_multiplier: n })}
                          />
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
                            <button
                              className="btn small"
                              disabled={Boolean(pending[`dis-${u.id}`])}
                              onClick={() => toggleDisabled(u)}
                            >
                              {pending[`dis-${u.id}`]
                                ? u.disabled ? 'Enabling…' : 'Disabling…'
                                : u.disabled ? 'Enable' : 'Disable'}
                            </button>
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </>
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

/** Role select with revert-on-failure: a plain controlled select keeps the
 *  rejected choice on screen until some other render happens, so the pick
 *  lives in local state and snaps back when the PATCH is refused (same
 *  contract EditableText follows with its boolean return). */
function RoleSelect({
  user,
  disabled,
  onPatch,
}: {
  user: User;
  disabled: boolean;
  onPatch: (next: Role) => Promise<boolean>;
}) {
  const [val, setVal] = useState<Role>(user.role);
  useEffect(() => setVal(user.role), [user.role]);
  return (
    <select
      className="input input-inline"
      value={val}
      disabled={disabled}
      onChange={(e) => {
        const next = e.target.value as Role;
        setVal(next);
        void onPatch(next).then((ok) => {
          if (!ok) setVal(user.role);
        });
      }}
      aria-label={`Role of ${user.username}`}
    >
      <option value="user">user</option>
      <option value="admin">admin</option>
    </select>
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
    if (passwordTooShort(password)) return;
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
        {password !== '' && passwordTooShort(password) && (
          <p className="dim">Password must be at least {MIN_PASSWORD_LEN} characters.</p>
        )}
        <div className="modal-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            type="submit"
            className="btn primary"
            disabled={busy || passwordTooShort(password)}
          >
            {busy ? 'Saving…' : 'Set password'}
          </button>
        </div>
      </form>
    </Modal>
  );
}
