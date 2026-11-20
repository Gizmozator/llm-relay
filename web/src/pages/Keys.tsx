import { useState } from 'react';
import type { FormEvent } from 'react';
import { api, errMsg } from '../api';
import type { CreatedKey } from '../api';
import { copyToClipboard, fmtDateTime } from '../format';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import {
  Banner,
  ConfirmButton,
  CopyField,
  Empty,
  Field,
  Loading,
  Modal,
} from '../components/ui';

export default function KeysPage() {
  const toast = useToast();
  const keys = useLoad(() => api.keys().then((r) => r.keys), []);
  const info = useLoad(() => api.info(), []);
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<CreatedKey | null>(null);
  const [copied, setCopied] = useState(false);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;
    setBusy(true);
    try {
      const k = await api.createKey(trimmed);
      setCreated(k);
      setCopied(false);
      setName('');
      keys.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (id: number) => {
    try {
      await api.deleteKey(id);
      toast.push('success', 'Key revoked.');
      keys.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    }
  };

  const copy = async () => {
    if (!created) return;
    const ok = await copyToClipboard(created.key);
    setCopied(ok);
    toast.push(ok ? 'success' : 'error', ok ? 'Key copied to clipboard.' : 'Copy failed — select the key manually.');
  };

  return (
    <div className="page">
      <div className="page-head">
        <h2>My API keys</h2>
      </div>

      <section className="panel">
        <h3>API base URL for your apps</h3>
        {info.loading && !info.data ? (
          <Loading />
        ) : info.error ? (
          <Banner>{info.error}</Banner>
        ) : (
          <CopyField
            value={info.data?.relay_url ?? ''}
            hint="Not configured by the admin (PUBLIC_RELAY_URL)."
          />
        )}
      </section>

      <form className="panel form-row" onSubmit={create}>
        <Field label="New key name">
          <input
            className="input"
            placeholder="e.g. laptop"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </Field>
        <button className="btn primary" type="submit" disabled={busy || !name.trim()}>
          {busy ? 'Creating…' : 'Create key'}
        </button>
      </form>

      <section className="panel">
        {keys.loading && !keys.data ? (
          <Loading />
        ) : keys.error ? (
          <Banner>{keys.error}</Banner>
        ) : !keys.data || keys.data.length === 0 ? (
          <Empty label="No API keys yet. Create one above." />
        ) : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Prefix</th>
                  <th>Created</th>
                  <th>Last used</th>
                  <th>Status</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {keys.data.map((k) => (
                  <tr key={k.id}>
                    <td>{k.name}</td>
                    <td className="mono">{k.prefix}…</td>
                    <td className="nowrap">{fmtDateTime(k.created_at)}</td>
                    <td className="nowrap">{k.last_used_at ? fmtDateTime(k.last_used_at) : 'never'}</td>
                    <td>
                      {k.revoked_at ? (
                        <span className="badge err" title={fmtDateTime(k.revoked_at)}>
                          revoked
                        </span>
                      ) : (
                        <span className="badge ok">active</span>
                      )}
                    </td>
                    <td className="actions">
                      {!k.revoked_at && (
                        <ConfirmButton label="Revoke" onConfirm={() => revoke(k.id)} />
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {created && (
        <Modal title={`Key created — ${created.name}`} onClose={() => setCreated(null)}>
          <Banner kind="info">
            This is the only time the full key is shown. Copy it now and store it safely.
          </Banner>
          <div className="key-box">
            <code className="key-text mono">{created.key}</code>
            <button className="btn" onClick={copy}>
              {copied ? 'Copied ✓' : 'Copy'}
            </button>
          </div>
          <div className="modal-actions">
            <button className="btn primary" onClick={() => setCreated(null)}>
              Done
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}
