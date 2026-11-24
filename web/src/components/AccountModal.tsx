import { useState } from 'react';
import type { FormEvent } from 'react';
import { api, errMsg, MIN_PASSWORD_LEN, passwordTooShort } from '../api';
import { useAuth } from '../auth';
import { useToast } from './Toast';
import { Banner, Field, Modal } from './ui';

/** Account modal: display name + change password. */
export default function AccountModal({ onClose }: { onClose: () => void }) {
  const { user, refreshUser } = useAuth();
  const toast = useToast();

  // --- display name ---
  const [displayName, setDisplayName] = useState(user?.display_name ?? '');
  const [nameBusy, setNameBusy] = useState(false);
  const [nameError, setNameError] = useState<string | null>(null);

  const saveName = async (e: FormEvent) => {
    e.preventDefault();
    const next = displayName.trim();
    if (next.length > 64) {
      setNameError('Display name must be at most 64 characters.');
      return;
    }
    setNameBusy(true);
    setNameError(null);
    try {
      const updated = await api.patchMe(next);
      refreshUser(updated);
      toast.push('success', 'Display name updated.');
    } catch (err: unknown) {
      setNameError(errMsg(err));
    } finally {
      setNameBusy(false);
    }
  };

  // --- change password ---
  const [oldPw, setOldPw] = useState('');
  const [newPw, setNewPw] = useState('');
  const [confirmPw, setConfirmPw] = useState('');
  const [pwBusy, setPwBusy] = useState(false);
  const [pwError, setPwError] = useState<string | null>(null);

  const changePw = async (e: FormEvent) => {
    e.preventDefault();
    setPwError(null);
    if (passwordTooShort(newPw)) {
      setPwError(`New password must be at least ${MIN_PASSWORD_LEN} characters.`);
      return;
    }
    if (newPw !== confirmPw) {
      setPwError('New passwords do not match.');
      return;
    }
    setPwBusy(true);
    try {
      await api.changePassword(oldPw, newPw);
      setOldPw('');
      setNewPw('');
      setConfirmPw('');
      toast.push('success', 'Password changed.');
    } catch (err: unknown) {
      setPwError(errMsg(err));
    } finally {
      setPwBusy(false);
    }
  };

  return (
    <Modal title="Account" onClose={onClose}>
      <form className="form-stack account-section" onSubmit={saveName}>
        <h4>Display name</h4>
        {nameError && <Banner>{nameError}</Banner>}
        <Field label="Shown in the dashboard">
          <input
            className="input"
            maxLength={64}
            value={displayName}
            placeholder={user?.username ?? ''}
            onChange={(e) => setDisplayName(e.target.value)}
          />
        </Field>
        <div>
          <button className="btn primary" type="submit" disabled={nameBusy}>
            {nameBusy ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>

      <hr className="divider" />

      <form className="form-stack account-section" onSubmit={changePw}>
        <h4>Change password</h4>
        {pwError && <Banner>{pwError}</Banner>}
        <Field label="Current password">
          <input
            className="input"
            type="password"
            autoComplete="current-password"
            value={oldPw}
            onChange={(e) => setOldPw(e.target.value)}
          />
        </Field>
        <Field label="New password (min 8 chars)">
          <input
            className="input"
            type="password"
            autoComplete="new-password"
            value={newPw}
            onChange={(e) => setNewPw(e.target.value)}
          />
        </Field>
        <Field label="Confirm new password">
          <input
            className="input"
            type="password"
            autoComplete="new-password"
            value={confirmPw}
            onChange={(e) => setConfirmPw(e.target.value)}
          />
        </Field>
        <div>
          <button className="btn primary" type="submit" disabled={pwBusy || !oldPw || !newPw || !confirmPw}>
            {pwBusy ? 'Changing…' : 'Change password'}
          </button>
        </div>
      </form>
    </Modal>
  );
}
