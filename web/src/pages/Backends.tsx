import { useEffect, useMemo, useState } from 'react';
import type { FormEvent } from 'react';
import { api, errMsg, isSyntheticBackendUrl } from '../api';
import type { Backend, CatalogModel, ProbeResult } from '../api';
import { fmtDateTime } from '../format';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import { Banner, ConfirmButton, Empty, Field, Loading, Modal, Toggle } from '../components/ui';

export default function BackendsPage() {
  const toast = useToast();
  const backends = useLoad(() => api.backends().then((r) => r.backends), []);
  const models = useLoad(() => api.models().then((r) => r.models), []);

  // Keep the live status badge honest without a manual refresh; the shared
  // health cache caps the real probes at one per backend per minute, so this
  // poll is cheap. Modals hold their own copies, so a reload never disturbs
  // editing.
  useEffect(() => {
    const t = setInterval(() => backends.reload(), 10_000);
    return () => clearInterval(t);
  }, [backends.reload]);

  const [showAdd, setShowAdd] = useState(false);
  const [editTarget, setEditTarget] = useState<Backend | null>(null);
  const [syncTarget, setSyncTarget] = useState<Backend | null>(null);

  const patch = async (id: number, p: { enabled?: boolean; draining?: boolean }) => {
    try {
      await api.patchBackend(id, p);
      backends.reload();
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
      backends.reload();
    }
  };

  const remove = async (id: number) => {
    try {
      await api.deleteBackend(id);
      toast.push('success', 'Backend deleted.');
      backends.reload();
      models.reload();
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
    }
  };

  const list = backends.data ?? [];

  return (
    <div className="page">
      <div className="page-head">
        <h2>Backends</h2>
        <button className="btn primary" onClick={() => setShowAdd(true)}>
          + Add backend
        </button>
      </div>

      {backends.loading && !backends.data ? (
        <Loading />
      ) : backends.error ? (
        <Banner>{backends.error}</Banner>
      ) : (
        <section className="panel">
          {list.length === 0 ? (
            <Empty label="No backends configured yet." />
          ) : (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Base URL</th>
                    <th>Enabled</th>
                    <th>Draining</th>
                    <th>Status</th>
                    <th>Server version</th>
                    <th>Last probe</th>
                    <th className="num">Models</th>
                    <th></th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((b) => {
                    // Synthetic (federation-materialized) backends: their
                    // health IS the channel (no HTTP to probe), and their
                    // rows are owned by the catalog sync, not by sync/edit.
                    const fed = isSyntheticBackendUrl(b.base_url);
                    return (
                    <tr key={b.id}>
                      <td>
                        {b.name}
                        {fed && (
                          <span className="badge fed" title="Materialized by federation — health is the peer channel">
                            federation
                          </span>
                        )}
                      </td>
                      <td className="mono">{b.base_url}</td>
                      <td>
                        <Toggle checked={b.enabled} onChange={(v) => patch(b.id, { enabled: v })} />
                      </td>
                      <td>
                        <span title="Drain: refuse new sessions, let running conversations finish (graceful shutdown)">
                          <Toggle checked={b.draining} onChange={(v) => patch(b.id, { draining: v })} />
                        </span>
                      </td>
                      <td>
                        {b.online ? (
                          <span className="badge ok" title="Live reachability, re-checked at most once a minute">
                            ok
                          </span>
                        ) : (
                          <span className="badge err" title={b.health_error || 'unreachable'}>
                            offline
                          </span>
                        )}
                        {b.has_key && <span className="tag" title="API key stored">key</span>}
                      </td>
                      <td className="mono">{b.server_version ?? '—'}</td>
                      <td className="nowrap" title={b.last_probe_error ?? undefined}>
                        {fmtDateTime(b.last_probe_at)}
                      </td>
                      <td className="num">{b.model_count}</td>
                      <td className="actions">
                        {/* Synthetic backends expose nothing editable: sync
                            and probe have nothing to talk to, an Edit would
                            repoint the base_url the materializer keys on
                            (validation failure or a detached federation),
                            and Delete 409s — the rows re-materialize
                            anyway on the next catalog frame. */}
                        {!fed && (
                          <>
                            <button className="btn small" onClick={() => setSyncTarget(b)}>
                              Sync models
                            </button>
                            <button className="btn small" onClick={() => setEditTarget(b)}>
                              Edit
                            </button>
                            <ConfirmButton label="Delete" onConfirm={() => remove(b.id)} />
                          </>
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
      )}

      {showAdd && (
        <BackendFormModal
          title="Add backend"
          onClose={() => setShowAdd(false)}
          onSubmit={async (name, base_url, api_key) => {
            await api.createBackend(name, base_url, api_key || undefined);
            toast.push('success', 'Backend added.');
            setShowAdd(false);
            backends.reload();
          }}
        />
      )}

      {editTarget && (
        <BackendFormModal
          title={`Edit backend — ${editTarget.name}`}
          backend={editTarget}
          onClose={() => setEditTarget(null)}
          onSubmit={async (name, base_url, api_key, clearKey) => {
            const patchBody: { name?: string; base_url?: string; api_key?: string } = {
              name,
              base_url,
            };
            if (clearKey) patchBody.api_key = '';
            else if (api_key) patchBody.api_key = api_key;
            await api.patchBackend(editTarget.id, patchBody);
            toast.push('success', 'Backend updated.');
            setEditTarget(null);
            backends.reload();
          }}
        />
      )}

      {syncTarget && (
        <SyncModal
          backend={syncTarget}
          catalog={models.data ?? []}
          onClose={() => setSyncTarget(null)}
          onAdded={(count, skipped) => {
            toast.push(
              'success',
              skipped.length > 0
                ? `Added ${count} model(s); skipped ${skipped.length} (name already in catalog): ${skipped.join(', ')}.`
                : `Added ${count} model(s) to the catalog.`,
            );
            setSyncTarget(null);
            backends.reload();
            models.reload();
          }}
        />
      )}
    </div>
  );
}

// ---------- add / edit form ----------

function BackendFormModal({
  title,
  backend,
  onClose,
  onSubmit,
}: {
  title: string;
  backend?: Backend;
  onClose: () => void;
  onSubmit: (name: string, base_url: string, apiKey: string, clearKey?: boolean) => Promise<void>;
}) {
  const [name, setName] = useState(backend?.name ?? '');
  const [baseUrl, setBaseUrl] = useState(backend?.base_url ?? '');
  const [apiKey, setApiKey] = useState('');
  const [clearKey, setClearKey] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await onSubmit(name.trim(), baseUrl.trim(), apiKey.trim(), clearKey);
    } catch (err: unknown) {
      setError(errMsg(err));
      setBusy(false);
    }
  };

  return (
    <Modal title={title} onClose={onClose}>
      <form onSubmit={submit} className="form-stack">
        {error && <Banner>{error}</Banner>}
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} autoFocus />
        </Field>
        <Field label="Base URL">
          <input
            className="input mono"
            placeholder="http://localhost:8080"
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
          />
        </Field>
        <Field label={backend?.has_key ? 'API key (leave blank to keep)' : 'API key (optional)'}>
          <input
            className="input"
            type="password"
            placeholder={backend?.has_key ? '••••••••' : 'sk-…'}
            value={apiKey}
            disabled={clearKey}
            onChange={(e) => setApiKey(e.target.value)}
          />
        </Field>
        {backend?.has_key && (
          <label className="checkbox-line">
            <input type="checkbox" checked={clearKey} onChange={(e) => setClearKey(e.target.checked)} />
            <span>Remove stored API key</span>
          </label>
        )}
        <div className="modal-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn primary" disabled={busy || !name.trim() || !baseUrl.trim()}>
            {busy ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ---------- model sync ----------

function SyncModal({
  backend,
  catalog,
  onClose,
  onAdded,
}: {
  backend: Backend;
  catalog: CatalogModel[];
  onClose: () => void;
  onAdded: (count: number, skipped: string[]) => void;
}) {
  const [probe, setProbe] = useState<ProbeResult | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [checked, setChecked] = useState<Set<string>>(new Set());
  const [adding, setAdding] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api
      .probeBackend(backend.id)
      .then((p) => {
        if (cancelled) return;
        setProbe(p);
        // Pre-check every newly discovered model (not yet in the catalog).
        setChecked(new Set((p.models ?? []).filter((m) => !m.in_catalog).map((m) => m.id)));
      })
      .catch((e: unknown) => {
        if (!cancelled) setError(errMsg(e));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [backend.id]);

  // The backend sends null when the probe failed or found nothing.
  const discovered = useMemo(() => probe?.models ?? [], [probe]);

  const discoveredIds = useMemo(() => new Set(discovered.map((m) => m.id)), [discovered]);

  // In the catalog for this backend but no longer offered upstream. Only a
  // successful probe tells us what upstream actually offers; a failed probe
  // would make every catalog model look vanished.
  const vanished = useMemo(
    () =>
      probe?.ok
        ? catalog.filter(
            (m) => m.backend_id === backend.id && !discoveredIds.has(m.upstream_model),
          )
        : [],
    [probe, catalog, backend.id, discoveredIds],
  );

  const toggle = (id: string) => {
    setChecked((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const addSelected = async () => {
    const ids = [...checked];
    if (ids.length === 0) return;
    setAdding(true);
    try {
      const res = await api.addBackendModels(backend.id, ids);
      onAdded(res.created.length, res.skipped ?? []);
    } catch (e: unknown) {
      setError(errMsg(e));
      setAdding(false);
    }
  };

  const newCount = discovered.filter((m) => !m.in_catalog).length;

  return (
    <Modal title={`Sync models — ${backend.name}`} onClose={onClose} wide>
      {loading ? (
        <Loading label="Probing backend…" />
      ) : (
        <>
          {error && <Banner>{error}</Banner>}
          {probe && !probe.ok && <Banner>{probe.error || 'Probe failed.'}</Banner>}
          {probe && probe.ok && (
            <Banner kind="info">
              Connected{probe.server_version ? ` — server version ${probe.server_version}` : ''}.{' '}
              {discovered.length} model(s) discovered, {newCount} new.
            </Banner>
          )}

          {probe && probe.ok && discovered.length === 0 && !error && (
            <Empty label="No models discovered." />
          )}

          {probe && discovered.length > 0 && (
            <div className="table-scroll sync-list">
              <table>
                <thead>
                  <tr>
                    <th></th>
                    <th>Upstream model id</th>
                    <th>Status</th>
                  </tr>
                </thead>
                <tbody>
                  {discovered.map((m) => (
                    <tr key={m.id} className={m.in_catalog ? 'row-muted' : ''}>
                      <td>
                        <input
                          type="checkbox"
                          checked={checked.has(m.id)}
                          disabled={m.in_catalog}
                          onChange={() => toggle(m.id)}
                          aria-label={`Select ${m.id}`}
                        />
                      </td>
                      <td className="mono">{m.id}</td>
                      <td>
                        {m.in_catalog ? (
                          <span className="badge ok">in catalog</span>
                        ) : (
                          <span className="badge new">new</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {vanished.length > 0 && (
            <div className="vanished">
              <h4>⚠ In catalog but not discovered upstream</h4>
              {vanished.map((m) => (
                <div key={m.id} className="vanished-row">
                  <span className="mono">{m.upstream_model}</span>
                  <span className="dim">as “{m.name}”</span>
                </div>
              ))}
            </div>
          )}

          <div className="modal-actions">
            <button className="btn" onClick={onClose}>
              Close
            </button>
            <button
              className="btn primary"
              disabled={adding || checked.size === 0}
              onClick={addSelected}
            >
              {adding ? 'Adding…' : `Add selected (${checked.size})`}
            </button>
          </div>
        </>
      )}
    </Modal>
  );
}
