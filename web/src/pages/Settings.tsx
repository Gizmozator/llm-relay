import { useState } from 'react';
import type { FormEvent } from 'react';
import { api, errMsg } from '../api';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import { Banner, CopyField, Field, Loading } from '../components/ui';

export default function SettingsPage() {
  const toast = useToast();
  const settings = useLoad(() => api.settings(), []);
  const info = useLoad(() => api.info(), []);
  const [currency, setCurrency] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const current = currency ?? settings.data?.currency ?? '';
  const dirty = currency !== null && currency !== (settings.data?.currency ?? '');

  const save = async (e: FormEvent) => {
    e.preventDefault();
    const code = current.trim().toUpperCase();
    if (!/^[A-Z]{3}$/.test(code)) {
      toast.push('error', 'Currency must be a 3-letter ISO code, e.g. EUR.');
      return;
    }
    setBusy(true);
    try {
      await api.putSettings(code);
      toast.push('success', `Currency set to ${code}.`);
      setCurrency(null);
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
    </div>
  );
}
