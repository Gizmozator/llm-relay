import { useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router-dom';
import { api, errMsg } from '../api';
import { copyToClipboard, fmtAgo, fmtDateTime } from '../format';
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

/** A secret shown exactly once (API key or server token share this modal). */
interface ShownSecret {
  title: string;
  secret: string;
}

type CredType = 'key' | 'peer';

export default function KeysPage() {
  const toast = useToast();
  const keys = useLoad(() => api.keys().then((r) => r.keys), []);
  const peers = useLoad(() => api.myPeers().then((r) => r.peers), []);
  const info = useLoad(() => api.info(), []);
  // Server tokens only mean something while this instance is a hub (only a
  // hub accepts federation channels), so the choice follows the role.
  const fedRole = useLoad(() => api.fedRole(), []);
  const isHub = fedRole.data?.role === 'hub';

  const [credType, setCredType] = useState<CredType>('key');
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  // Row actions in flight, keyed per action+id: a second click while a
  // rotate/revoke/delete round-trip is still running would double-submit
  // (rotate mints a second token, delete 404s noisily).
  const [pending, setPending] = useState<Record<string, boolean>>({});
  const begin = (tag: string) => {
    if (pending[tag]) return false;
    setPending((p) => ({ ...p, [tag]: true }));
    return true;
  };
  const end = (tag: string) =>
    setPending((p) => {
      const { [tag]: _drop, ...rest } = p;
      return rest;
    });
  const [shown, setShown] = useState<ShownSecret | null>(null);
  const [copied, setCopied] = useState(false);

  const effectiveType: CredType = isHub ? credType : 'key';
  const submitLabel =
    busy
      ? 'Creating…'
      : effectiveType === 'peer'
        ? 'Create server token'
        : 'Create key';

  const create = async (e: FormEvent) => {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed) return;
    setBusy(true);
    try {
      if (effectiveType === 'peer') {
        const r = await api.createPeer(trimmed);
        setShown({ title: `Server token created — ${r.peer.name}`, secret: r.token });
        peers.reload();
      } else {
        const k = await api.createKey(trimmed);
        setShown({ title: `Key created — ${k.name}`, secret: k.key });
        keys.reload();
      }
      setCopied(false);
      setName('');
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (id: number) => {
    if (!begin(`revoke-${id}`)) return;
    try {
      await api.deleteKey(id);
      toast.push('success', 'Key revoked.');
      keys.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      end(`revoke-${id}`);
    }
  };

  const rotatePeer = async (id: number, peerName: string) => {
    if (!begin(`rotate-${id}`)) return;
    try {
      const r = await api.rotateMyPeer(id);
      setShown({ title: `Token rotated — ${peerName}`, secret: r.token });
      setCopied(false);
      peers.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      end(`rotate-${id}`);
    }
  };

  const deletePeer = async (id: number) => {
    if (!begin(`delete-${id}`)) return;
    try {
      await api.deleteMyPeer(id);
      toast.push('success', 'Server removed.');
      peers.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      end(`delete-${id}`);
    }
  };

  const copy = async () => {
    if (!shown) return;
    const ok = await copyToClipboard(shown.secret);
    setCopied(ok);
    toast.push(ok ? 'success' : 'error', ok ? 'Copied to clipboard.' : 'Copy failed — select the value manually.');
  };

  return (
    <div className="page">
      <div className="page-head">
        <h2>My API keys</h2>
      </div>

      {/* fedRole drives the server-token UI; on failure the conservative
          default (not a hub) applies, but the operator must know why the
          federation controls are missing rather than silently misled. */}
      {fedRole.error && (
        <Banner>
          Could not load the federation role ({fedRole.error}) — assuming this relay is
          not a hub, so server-token controls are hidden.
        </Banner>
      )}

      <section className="panel">
        <h3>API base URL for your apps</h3>
        {info.loading && !info.data ? (
          <Loading />
        ) : info.error ? (
          <Banner>{info.error}</Banner>
        ) : (
          <CopyField
            value={info.data?.relay_url ? `${info.data.relay_url.replace(/\/+$/, '')}/v1` : ''}
            hint="Not configured by the admin (PUBLIC_RELAY_URL)."
          />
        )}
      </section>

      <form className="panel form-row" onSubmit={create}>
        {isHub && (
          <Field label="Credential type">
            <select
              className="input"
              value={effectiveType}
              onChange={(e) => setCredType(e.target.value as CredType)}
            >
              <option value="key">API key (sk-…)</option>
              <option value="peer">Server token (fed-…)</option>
            </select>
          </Field>
        )}
        <Field label={effectiveType === 'peer' ? 'New server name' : 'New key name'}>
          <input
            className="input"
            placeholder={effectiveType === 'peer' ? 'e.g. home-server' : 'e.g. laptop'}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </Field>
        <button className="btn primary" type="submit" disabled={busy || !name.trim()}>
          {submitLabel}
        </button>
      </form>
      {isHub && effectiveType === 'peer' && (
        <p className="dim">
          A server token lets one of your relays (a spoke) connect to this hub —
          see the <Link to="/setup">setup guide</Link>. It cannot be used for inference.
        </p>
      )}

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
                        <ConfirmButton
                          label="Revoke"
                          disabled={Boolean(pending[`revoke-${k.id}`])}
                          onConfirm={() => revoke(k.id)}
                        />
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {(isHub || (peers.data?.length ?? 0) > 0) && (
        <section className="panel">
          <h3>My servers</h3>
          {peers.loading && !peers.data ? (
            <Loading />
          ) : peers.error ? (
            <Banner>{peers.error}</Banner>
          ) : !peers.data || peers.data.length === 0 ? (
            <Empty label="No server tokens yet. Create one above (type: Server token)." />
          ) : (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Created</th>
                    <th>Last seen</th>
                    <th>Spoke version</th>
                    <th>Status</th>
                    <th></th>
                  </tr>
                </thead>
                <tbody>
                  {peers.data.map((p) => (
                    <tr key={p.id}>
                      <td>{p.name}</td>
                      <td className="nowrap">{fmtDateTime(p.created_at)}</td>
                      <td className="nowrap">{p.last_seen ? fmtAgo(p.last_seen) : 'never'}</td>
                      <td className="mono">{p.version || '—'}</td>
                      <td>
                        {p.enabled ? (
                          <span className="badge ok">enabled</span>
                        ) : (
                          <span className="badge err">disabled</span>
                        )}
                      </td>
                      <td className="actions">
                        <button
                          className="btn small"
                          disabled={Boolean(pending[`rotate-${p.id}`])}
                          onClick={() => rotatePeer(p.id, p.name)}
                        >
                          {pending[`rotate-${p.id}`] ? 'Rotating…' : 'Rotate token'}
                        </button>
                        <ConfirmButton
                          label="Delete"
                          disabled={Boolean(pending[`delete-${p.id}`])}
                          onConfirm={() => deletePeer(p.id)}
                        />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      )}

      {shown && (
        <Modal title={shown.title} onClose={() => setShown(null)}>
          <Banner kind="info">
            This is the only time the full value is shown. Copy it now and store it
            safely.
          </Banner>
          <div className="key-box">
            <code className="key-text mono">{shown.secret}</code>
            <button className="btn" onClick={copy}>
              {copied ? 'Copied ✓' : 'Copy'}
            </button>
          </div>
          <div className="modal-actions">
            <button className="btn primary" onClick={() => setShown(null)}>
              Done
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}
