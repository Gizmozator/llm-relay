import { Fragment, useMemo, useState } from 'react';
import { api, errMsg, isSyntheticBackendUrl } from '../api';
import type { Backend, CatalogModel, FedPeer } from '../api';
import { fmtAgo, fmtDateTime, fmtInt, fmtMoney, inNewTokens } from '../format';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import {
  Banner,
  ConfirmButton,
  CopyField,
  Empty,
  Loading,
  Modal,
  StatsWindowSelect,
  Toggle,
} from '../components/ui';

/**
 * Servers (admin): the peer registry — every server token a spoke dials this
 * hub with, its live state, what it shares, and the federation ledger
 * (per-user fairness + per-peer rollup). On a spoke/standalone box the peer
 * table is inert (channels only live at a hub), so the page says so.
 */
export default function ServersPage() {
  const toast = useToast();
  const peers = useLoad(() => api.adminPeers().then((r) => r.peers), []);
  const settings = useLoad(() => api.fedSettings(), []);
  const [days, setDays] = useState(30);
  const stats = useLoad(() => api.fedAdminStats(days), [days]);
  // The catalog + backends drive the expandable shared-model list: the wire
  // carries only the count, the names are derived from fed_visibility +
  // allowlists, filtered by the same servability rule the server counts with.
  const models = useLoad(() => api.models().then((r) => r.models), []);
  const backends = useLoad(() => api.backends().then((r) => r.backends), []);

  const [rotateToken, setRotateToken] = useState<{ name: string; token: string } | null>(null);
  const [expanded, setExpanded] = useState<number | null>(null);

  const patch = async (id: number, enabled: boolean) => {
    try {
      await api.adminPatchPeer(id, enabled);
      peers.reload();
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
      peers.reload();
    }
  };

  const rotate = async (p: FedPeer) => {
    try {
      const r = await api.adminRotatePeer(p.id);
      setRotateToken({ name: p.name, token: r.token });
      peers.reload();
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
    }
  };

  const remove = async (p: FedPeer) => {
    try {
      await api.adminDeletePeer(p.id);
      toast.push('success', `Server “${p.name}” removed.`);
      peers.reload();
      stats.reload();
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
    }
  };

  const backendById = useMemo(
    () => new Map<number, Backend>((backends.data ?? []).map((b) => [b.id, b])),
    [backends.data],
  );

  // Mirror the server's SharedModels SQL (store.sharedModelsExpr): servable
  // (model AND backend enabled) and never a synthetic backend — the hub does
  // not re-share materialized peer rows. Keeps the expanded list honest
  // against the Shared count.
  const sharedNames = (p: FedPeer, list: CatalogModel[]) =>
    list.filter((m) => {
      const b = backendById.get(m.backend_id);
      return (
        m.enabled &&
        !!b?.enabled &&
        !isSyntheticBackendUrl(b.base_url) &&
        (m.fed_visibility === 'all' ||
          (m.fed_visibility === 'allowlist' && m.fed_peer_ids.includes(p.id)))
      );
    });

  const currency = useLoad(() => api.settings(), []).data?.currency ?? '';

  return (
    <div className="page">
      <div className="page-head">
        <h2>Servers</h2>
        <StatsWindowSelect value={days} onChange={setDays} />
      </div>

      {settings.data && settings.data.role !== 'hub' && (
        <Banner kind="info">
          This instance is <b>{settings.data.role}</b> — peer channels only live
          on a hub, so this registry is inert here.
        </Banner>
      )}

      <section className="panel">
        {peers.loading && !peers.data ? (
          <Loading />
        ) : peers.error ? (
          <Banner>{peers.error}</Banner>
        ) : !peers.data || peers.data.length === 0 ? (
          <Empty label="No servers enrolled yet. Users create server tokens on their My keys page." />
        ) : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Owner</th>
                  <th>Status</th>
                  <th>Last seen</th>
                  <th>Version</th>
                  <th className="num">Shared</th>
                  <th>Enabled</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {peers.data.map((p) => {
                  const names = sharedNames(p, models.data ?? []);
                  return (
                    <Fragment key={p.id}>
                      <tr>
                        <td>
                          <button
                            className="btn small"
                            title="Show which models this peer may see"
                            onClick={() => setExpanded(expanded === p.id ? null : p.id)}
                          >
                            {expanded === p.id ? '▾' : '▸'}
                          </button>{' '}
                          {p.name}
                        </td>
                        <td>{p.owner_name}</td>
                        <td>
                          {p.online ? (
                            <span className="badge ok">online</span>
                          ) : (
                            <span className="badge err">offline</span>
                          )}
                        </td>
                        <td className="nowrap" title={p.last_seen ? fmtDateTime(p.last_seen) : undefined}>
                          {p.last_seen ? fmtAgo(p.last_seen) : 'never'}
                        </td>
                        <td className="mono">{p.version || '—'}</td>
                        <td className="num">{p.shared_models}</td>
                        <td>
                          <Toggle
                            checked={p.enabled}
                            ariaLabel={`Enable server ${p.name}`}
                            onChange={(v) => patch(p.id, v)}
                          />
                        </td>
                        <td className="actions">
                          <button className="btn small" onClick={() => rotate(p)}>
                            Rotate token
                          </button>
                          <ConfirmButton label="Delete" onConfirm={() => remove(p)} />
                        </td>
                      </tr>
                      {expanded === p.id && (
                        <tr className="sub-row">
                          <td colSpan={8}>
                            {/* A failed catalog/backend load must never read
                                as “nothing shared” — the list needs both. */}
                            {!models.data || !backends.data ? (
                              models.error || backends.error ? (
                                <Banner>Could not load the model catalog — shared list unavailable.</Banner>
                              ) : (
                                <Loading label="Loading catalog…" />
                              )
                            ) : names.length === 0 ? (
                              <span className="dim">No models shared with this peer.</span>
                            ) : (
                              names.map((m) => (
                                <span className="tag" key={m.id} title={`federation: ${m.fed_visibility}`}>
                                  {m.name}
                                </span>
                              ))
                            )}
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>

      {stats.loading && !stats.data ? (
        <Loading />
      ) : stats.error ? (
        <Banner>{stats.error}</Banner>
      ) : stats.data?.role === 'hub' ? (
        <>
          <section className="panel">
            <h3>Fairness — served vs consumed per user (last {days} days)</h3>
            {stats.data.users.length === 0 ? (
              <Empty label="No federation traffic in this window." />
            ) : (
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th>User</th>
                      <th className="num">Served req</th>
                      <th className="num">Served cost</th>
                      <th className="num">Consumed req</th>
                      <th className="num">Consumed cost</th>
                    </tr>
                  </thead>
                  <tbody>
                    {stats.data.users.map((u) => (
                      <tr key={u.user_id}>
                        <td>{u.user}</td>
                        <td className="num">{fmtInt(u.served.requests)}</td>
                        <td className="num money">{fmtMoney(u.served.cost_micro, currency)}</td>
                        <td className="num">{fmtInt(u.consumed.requests)}</td>
                        <td className="num money">{fmtMoney(u.consumed.cost_micro, currency)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>

          <section className="panel">
            <h3>Per server (last {days} days)</h3>
            {stats.data.peers.length === 0 ? (
              <Empty label="No federation traffic in this window." />
            ) : (
              <>
                <div className="table-scroll">
                  <table>
                    <thead>
                      <tr>
                        <th>Server</th>
                        <th className="num">Served req</th>
                        <th className="num">Served cost</th>
                        <th className="num">Consumed req *</th>
                        <th className="num">Consumed cost *</th>
                      </tr>
                    </thead>
                    <tbody>
                      {stats.data.peers.map((p) => (
                        <tr key={p.peer_id}>
                          <td>{p.peer}</td>
                          <td className="num">{fmtInt(p.served.requests)}</td>
                          <td className="num money">{fmtMoney(p.served.cost_micro, currency)}</td>
                          <td className="num">{fmtInt(p.consumed.requests)}</td>
                          <td className="num money">{fmtMoney(p.consumed.cost_micro, currency)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                <p className="dim footnote">
                  * owner-level: a server’s consumed figures are every consumed row of its{' '}
                  <b>owner</b> — an owner with several servers carries the same total
                  under each, so these columns must not be summed.
                </p>
              </>
            )}
          </section>
        </>
      ) : stats.data?.role === 'spoke' ? (
        <section className="panel">
          <h3>This box served (last {days} days)</h3>
          {stats.data.served_by_source.length === 0 ? (
            <Empty label="Nothing served to the hub in this window." />
          ) : (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Hub user</th>
                    <th className="num">Requests</th>
                    <th className="num">In new</th>
                    <th className="num">Out</th>
                    <th className="num">Cost</th>
                  </tr>
                </thead>
                <tbody>
                  {stats.data.served_by_source.map((s) => (
                    <tr key={s.fed_user}>
                      <td className="mono">{s.fed_user}</td>
                      <td className="num">{fmtInt(s.counts.requests)}</td>
                      <td className="num">{fmtInt(inNewTokens(s.counts))}</td>
                      <td className="num">{fmtInt(s.counts.completion_tokens)}</td>
                      <td className="num money">{fmtMoney(s.counts.cost_micro, currency)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      ) : (
        <section className="panel">
          <Empty label="This instance is standalone — no federation ledger." />
        </section>
      )}

      {rotateToken && (
        <Modal title={`Token rotated — ${rotateToken.name}`} onClose={() => setRotateToken(null)}>
          <Banner kind="info">
            The old token died with this rotation. The spoke must be updated with the
            new one before it can reconnect; this is the only time it is shown.
          </Banner>
          <CopyField value={rotateToken.token} hint="" label="New server token" />
          <div className="modal-actions">
            <button className="btn primary" onClick={() => setRotateToken(null)}>
              Done
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}
