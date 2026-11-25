import { useState } from 'react';
import type { FormEvent } from 'react';
import { api, errMsg } from '../api';
import type { TlsView } from '../api';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import { Banner, ConfirmButton, CopyField, Field, Loading } from '../components/ui';

export default function SettingsPage() {
  const toast = useToast();
  const settings = useLoad(() => api.settings(), []);
  const info = useLoad(() => api.info(), []);
  const [currency, setCurrency] = useState<string | null>(null);
  const [retention, setRetention] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);

  const current = currency ?? settings.data?.currency ?? '';
  const currentRetention = retention ?? settings.data?.retention_days ?? 0;
  const dirty =
    (currency !== null && currency !== (settings.data?.currency ?? '')) ||
    (retention !== null && retention !== (settings.data?.retention_days ?? 0));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    const code = current.trim().toUpperCase();
    if (!/^[A-Z]{3}$/.test(code)) {
      toast.push('error', 'Currency must be a 3-letter ISO code, e.g. EUR.');
      return;
    }
    setBusy(true);
    try {
      await api.putSettings(code, currentRetention);
      toast.push('success', 'Settings saved.');
      setCurrency(null);
      setRetention(null);
      settings.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="page">
      <div className="page-head">
        <h2>Settings</h2>
      </div>

      {settings.loading && !settings.data ? (
        <Loading />
      ) : settings.error ? (
        <Banner>{settings.error}</Banner>
      ) : (
        <>
          <form className="panel form-stack settings-form" onSubmit={save}>
            <Field label="Currency code (ISO 4217, shown with every cost)">
              <input
                className="input"
                maxLength={3}
                value={current}
                onChange={(e) => setCurrency(e.target.value.toUpperCase())}
              />
            </Field>
            <Field label="Usage data retention">
              <select
                className="input"
                value={currentRetention}
                onChange={(e) => setRetention(Number(e.target.value))}
              >
                <option value={0}>Keep forever (default)</option>
                <option value={30}>30 days</option>
                <option value={90}>90 days</option>
                <option value={180}>180 days</option>
                <option value={365}>365 days</option>
                <option value={730}>730 days</option>
              </select>
            </Field>
            <p className="dim">
              Usage rows older than the retention window are deleted once a day (and
              right after you lower it here). Keep forever never deletes anything; the
              database file keeps the space freed by purged rows for reuse.
            </p>
            <div>
              <button className="btn primary" type="submit" disabled={busy || !dirty}>
                {busy ? 'Saving…' : 'Save'}
              </button>
            </div>
          </form>

          <section className="panel">
            <h3>Public URLs</h3>
            {info.loading && !info.data ? (
              <Loading />
            ) : info.error ? (
              <Banner>{info.error}</Banner>
            ) : (
              <div className="form-stack">
                <CopyField
                  label="Relay URL (what users put in their apps)"
                  value={info.data?.relay_url ? `${info.data.relay_url.replace(/\/+$/, '')}/v1` : ''}
                  hint="Not configured by the admin (PUBLIC_RELAY_URL)."
                />
                <CopyField
                  label="Dashboard URL"
                  value={info.data?.dashboard_url ?? ''}
                  hint="Not configured by the admin (PUBLIC_DASHBOARD_URL)."
                />
              </div>
            )}
          </section>
        </>
      )}

      <TlsPanel />
    </div>
  );
}

/**
 * Instance TLS certificate (plan 2026-11-24-remote-tls-v1). The relay serves
 * its machine channels (federation + remote-backend connect) over TLS with
 * this cert; peers pin its fingerprint. A relay behind a TLS-terminating
 * reverse proxy with a real cert needs none of this — the note says so.
 */
function TlsPanel() {
  const toast = useToast();
  const tls = useLoad(() => api.tls(), []);
  const [hosts, setHosts] = useState('');
  const [busy, setBusy] = useState(false);
  const [certPem, setCertPem] = useState('');

  const generate = async (regenerate: boolean) => {
    const list = hosts
      .split(/[\n,]/)
      .map((h) => h.trim())
      .filter(Boolean);
    if (list.length === 0) {
      toast.push('error', 'Enter at least one host name or IP.');
      return;
    }
    setBusy(true);
    try {
      const res = await api.tlsGenerate(list);
      toast.push(
        'success',
        regenerate ? 'Certificate regenerated.' : 'Certificate generated.',
      );
      setCertPem(res.cert_pem);
      setHosts('');
      tls.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    setBusy(true);
    try {
      await api.tlsDelete();
      toast.push('success', 'Certificate deleted.');
      setCertPem('');
      tls.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  if (tls.loading && !tls.data) return <Loading />;

  const view: TlsView | null = tls.data;
  const configured = !!view?.configured;

  return (
    <section className="panel">
      <h3>TLS certificate</h3>
      <p className="dim">
        Machine channels (federation and remote backends) connect over TLS only. Generate a
        self-signed certificate here and give peers its fingerprint to pin, or leave this
        empty if a TLS-terminating reverse proxy already serves this relay with a real
        certificate (set <code className="mono">TRUST_PROXY_HEADERS=true</code> in that
        case).
      </p>

      {tls.error ? <Banner>{tls.error}</Banner> : null}

      {configured && view ? (
        <div className="form-stack">
          <CopyField
            label="Fingerprint (peers paste this as hub_pin / RELAY_PIN)"
            value={view.fingerprint ?? ''}
            hint="No fingerprint."
          />
          <div className="field">
            <span className="field-label">Host names (SANs)</span>
            <span>{(view.sans ?? []).join(', ') || '—'}</span>
          </div>
          <div className="field">
            <span className="field-label">Expires</span>
            <span>{view.not_after ?? '—'}</span>
          </div>
          <div className="field">
            <span className="field-label">Created</span>
            <span>{view.created_at ?? '—'}</span>
          </div>
          {certPem ? (
            <CopyField
              label="Certificate PEM (one-time — for an optional proxy install)"
              value={certPem}
              hint=""
            />
          ) : null}
          <div className="copy-row">
            <button
              className="btn"
              type="button"
              disabled={busy}
              onClick={() => generate(true)}
              title="Mints a new key pair; every peer must re-pin the new fingerprint."
            >
              {busy ? 'Working…' : 'Regenerate'}
            </button>
            <ConfirmButton label="Delete" onConfirm={remove} disabled={busy} />
          </div>
          <p className="dim">
            Regenerating replaces the key pair: existing connections are unaffected, but
            every peer must update its pin before it can reconnect. Deleting stops all
            TLS handshakes until a new certificate is generated.
          </p>
        </div>
      ) : (
        <div className="form-stack">
          <Field label="Host names / IPs (one per line — the names peers dial)">
            <textarea
              className="input mono"
              rows={3}
              placeholder={'relay.example.fi\n192.0.2.10'}
              value={hosts}
              onChange={(e) => setHosts(e.target.value)}
            />
          </Field>
          <div>
            <button className="btn primary" type="button" disabled={busy} onClick={() => generate(false)}>
              {busy ? 'Generating…' : 'Generate certificate'}
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
