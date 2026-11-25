import { Fragment, useEffect, useMemo, useState } from 'react';
import type { FormEvent } from 'react';
import { api, errMsg, isSyntheticBackendUrl, FED_ROLES } from '../api';
import type {
  Backend,
  CatalogModel,
  FedAdminStats,
  FedCounts,
  FedPeer,
  FedRole,
  FedSettings,
} from '../api';
import { fmtAgo, fmtDateTime, fmtInt, fmtMoney, inNewTokens } from '../format';
import { FairnessPanel } from '../components/FairnessPanel';
import { useFedRole } from '../fedrole';
import { useLoad } from '../useLoad';
import type { LoadState } from '../useLoad';
import { useToast } from '../components/Toast';
import {
  Banner,
  Card,
  ConfirmButton,
  CopyField,
  Empty,
  Field,
  Loading,
  Modal,
  StatsWindowSelect,
  Toggle,
  windowLabel,
  windowToQuery,
} from '../components/ui';
import type { StatsWindow } from '../components/ui';

/**
 * Federation (admin) — the one page for the whole feature. It used to be two
 * routes (Federation + Servers) that each loaded the same settings and ledger
 * and bounced a hub admin between them to do one job; they are merged here into
 * three role-aware sections:
 *
 *   1. Role & connection — the instance role switch, and for a spoke its
 *      dial-in config with a live channel status plus what it shares.
 *   2. Servers — the peer registry (tokens, online state, shared models,
 *      enable/rotate/delete). Hub only: peer channels only live at a hub.
 *   3. Ledger — the served/consumed rollup (per-user fairness + per-server on
 *      a hub; served-to-hub + consumed-locally on a spoke).
 *
 * Visibility follows the role: a hub sees all three, a spoke sees role +
 * connection + its ledger panels (no peer registry), a standalone sees the role
 * switch only. The role is read from the shared {@link useFedRole} context (the
 * same state the sidebar keys on) so a saved change re-shapes the page at once.
 */
export default function AdminFederationPage() {
  const toast = useToast();
  const fedRole = useFedRole();
  const settings = useLoad(() => api.fedSettings(), []);
  const currency = useLoad(() => api.settings(), []).data?.currency ?? '';
  const [role, setRole] = useState<FedRole | null>(null);
  const [hubUrl, setHubUrl] = useState<string | null>(null);
  const [hubToken, setHubToken] = useState<string | null>(null);
  const [hubPin, setHubPin] = useState<string | null>(null);
  const [reveal, setReveal] = useState(false);
  const [busy, setBusy] = useState(false);
  const [win, setWin] = useState<StatsWindow>({ kind: 'days', days: 30 });
  // Peer-registry UI state (moved from the old Servers page).
  const [rotateToken, setRotateToken] = useState<{ name: string; token: string } | null>(null);
  const [expanded, setExpanded] = useState<number | null>(null);

  const cur = settings.data;
  // The persisted role drives which sections show. Read it from the shared
  // context (the sidebar's source of truth, updated the instant a save lands);
  // the settings payload carries the same value and covers the first paint
  // before the context load resolves. The draft select overrides both so the
  // panels preview as you switch role before saving.
  const persistedRole = fedRole.role ?? cur?.role ?? 'standalone';
  const curRole = role ?? persistedRole;
  const curUrl = hubUrl ?? cur?.hub_url ?? '';
  const curToken = hubToken ?? cur?.hub_token ?? '';
  const curPin = hubPin ?? cur?.hub_pin ?? '';
  // "Dirty" is always measured against what the SERVER has (the settings
  // payload), never the context echo, so Save offers exactly the pending edit.
  const dirty =
    curRole !== cur?.role ||
    curUrl !== cur?.hub_url ||
    curToken !== cur?.hub_token ||
    curPin !== cur?.hub_pin;

  // The spoke's connection status rides the settings payload; poll it like
  // the Backends page polls health — the dialer reconnects on its own, the
  // page just has to follow. A failed poll keeps the last good payload on
  // screen (useLoad retains data on error) and rides a banner instead —
  // transient failures must not wipe a working page. Keyed on the
  // PERSISTED role (not the draft select): picking "spoke" in the dropdown
  // before saving must not start polling a role the server does not have.
  useEffect(() => {
    if (cur?.role !== 'spoke') return;
    const t = setInterval(() => settings.reload(), 10_000);
    return () => clearInterval(t);
  }, [cur?.role, settings.reload]);

  // Peer registry (hub only): the registry itself, plus the catalog + backends
  // that drive the expandable shared-model list (the wire carries only the
  // count; names derive from fed_visibility + allowlists, filtered by the same
  // servability rule the server counts with). Standalone/spoke never render the
  // registry, so these loads stay idle off a hub.
  const peers = useLoad(
    () => (curRole === 'hub' ? api.adminPeers().then((r) => r.peers) : Promise.resolve([])),
    [curRole],
  );
  const backends = useLoad(
    () => (curRole === 'hub' ? api.backends().then((r) => r.backends) : Promise.resolve([])),
    [curRole],
  );
  // The model catalog serves two sections: the spoke's shared list and the
  // hub's expandable per-peer list. Standalone needs neither.
  const models = useLoad(
    () => (curRole === 'standalone' ? Promise.resolve([]) : api.models().then((r) => r.models)),
    [curRole],
  );
  // The ledger: hub fairness + per-peer rollup, spoke served/consumed. One
  // load feeds both (the old pages each carried their own fedAdminStats call).
  const stats = useLoad(
    () => (curRole === 'standalone' ? Promise.resolve(null) : api.fedAdminStats(windowToQuery(win))),
    [curRole, win],
  );

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      const res = await api.putFedSettings(curRole, curUrl.trim(), curToken.trim(), curPin.trim());
      // Publish the role the server says it persisted: the sidebar menu and this
      // page's own sections are keyed on the shared role, so they follow the
      // save at once — no reload, and no stale entry left pointing at a section
      // that no longer applies.
      fedRole.saved(res.role);
      toast.push('success', 'Federation settings saved.');
      setRole(null);
      setHubUrl(null);
      setHubToken(null);
      setHubPin(null);
      settings.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      setBusy(false);
    }
  };

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
  // not re-share materialized peer rows. The allowlist names users and the
  // peer passes it through its owner; 'owner' rows need the backend's
  // fairness owner to be the peer's owner (ticket #21). Keeps the expanded
  // list honest against the Shared count.
  const sharedNames = (p: FedPeer, list: CatalogModel[]) =>
    list.filter((m) => {
      const b = backendById.get(m.backend_id);
      return (
        m.enabled &&
        !!b?.enabled &&
        !isSyntheticBackendUrl(b.base_url) &&
        (m.fed_visibility === 'all' ||
          (m.fed_visibility === 'allowlist' && m.fed_user_ids.includes(p.owner_user_id)) ||
          (m.fed_visibility === 'owner' && b.owner_user_id === p.owner_user_id))
      );
    });

  const spokeStatus = cur?.spoke;
  // A spoke without a usable dial-in target cannot connect: the URL must be
  // https (hard TLS rule, plan 2026-11-24-remote-tls-v1) and the token
  // non-empty (the server validates too, but the Save button should not
  // offer a guaranteed 400). The pin is optional (a CA-cert hub needs none).
  const spokeConfigValid =
    curRole !== 'spoke' ||
    (/^https:\/\/\S+$/i.test(curUrl.trim()) && curToken.trim() !== '');
  // The server ships fed_visibility='all' AND model-enabled AND backend-
  // enabled; the model payload carries the model flag only, so a model on
  // a disabled backend can linger in this list (residual over-count, the
  // hub-side share filter is the authority).
  const shared = (models.data ?? []).filter((m) => m.fed_visibility === 'all' && m.enabled);

  return (
    <div className="page">
      <div className="page-head">
        <h2>Federation</h2>
        {curRole !== 'standalone' && <StatsWindowSelect value={win} onChange={setWin} />}
      </div>

      {settings.loading && !settings.data ? (
        <Loading />
      ) : !settings.data ? (
        <Banner>{settings.error ?? 'Could not load federation settings.'}</Banner>
      ) : (
        <>
          {settings.error && (
            <Banner>
              Refresh failed — showing the last loaded values. {settings.error}
            </Banner>
          )}

          {/* ---- Section 1: Role & connection ---- */}
          <form className="panel form-stack settings-form" onSubmit={save}>
            <h3>Instance role</h3>
            <Field label="This relay acts as">
              <select
                className="input"
                value={curRole}
                onChange={(e) => setRole(e.target.value as FedRole)}
              >
                {FED_ROLES.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
            </Field>
            <p className="dim">
              <b>standalone</b> — no federation. <b>hub</b> — accepts other relays
              (servers) connecting here; manage them in the <b>Servers</b> section
              below. <b>spoke</b> — this box dials a hub and shares its opted-in
              models there.
            </p>

            {curRole === 'spoke' && (
              <>
                <Field label="Hub URL (the hub relay’s base URL)">
                  <input
                    className="input mono"
                    placeholder="https://hub.example.fi"
                    value={curUrl}
                    onChange={(e) => setHubUrl(e.target.value)}
                  />
                </Field>
                <Field label="Hub token (fed-… from the hub’s My keys page)">
                  <div className="copy-row">
                    <input
                      className="input mono"
                      type={reveal ? 'text' : 'password'}
                      value={curToken}
                      onChange={(e) => setHubToken(e.target.value)}
                    />
                    <button type="button" className="btn" onClick={() => setReveal((v) => !v)}>
                      {reveal ? 'Hide' : 'Show'}
                    </button>
                  </div>
                </Field>
                <Field label="Hub pin (optional — the hub cert fingerprint)">
                  <input
                    className="input mono"
                    placeholder="sha256:…"
                    value={curPin}
                    onChange={(e) => setHubPin(e.target.value)}
                  />
                </Field>
                <p className="dim">
                  Required when the hub uses a self-signed cert: paste the fingerprint from
                  the hub’s Settings → TLS page. Leave empty when the hub has a real CA cert
                  (a TLS-terminating proxy) — it verifies normally.
                </p>
                {!spokeConfigValid && (
                  <p className="dim">
                    Hub URL must start with https:// (federation requires TLS), and the hub
                    token is required — Save stays disabled until both are filled.
                  </p>
                )}
              </>
            )}

            <div>
              <button className="btn primary" type="submit" disabled={busy || !dirty || !spokeConfigValid}>
                {busy ? 'Saving…' : 'Save'}
              </button>
            </div>
          </form>

          {curRole === 'spoke' && (
            <section className="panel">
              <h3>Connection</h3>
              <SpokeConnection status={spokeStatus} />
            </section>
          )}

          {/* Version notes (plan 2026-11-25-version-awareness): connected
              instances running a different build. This server being the old
              one is the red all-user banner (Layout); here everything is a
              small admin note. */}
          {curRole !== 'standalone' && cur?.version_warnings && cur.version_warnings.length > 0 && (
            <Banner kind="info">
              Version mismatch on live channels:{' '}
              {cur?.version_warnings
                .map((w) => `${w.kind} “${w.name}” ${w.version} (${w.state} than this server)`)
                .join('; ')}
              {cur?.version_warnings.some((w) => w.state === 'newer') &&
                ' — this relay is the older one; update it.'}
            </Banner>
          )}

          {curRole === 'spoke' && (
            <section className="panel">
              <h3>Shared models</h3>
              {models.loading && !models.data ? (
                <Loading />
              ) : models.error && !models.data ? (
                <Banner>Could not load the model catalog: {models.error}</Banner>
              ) : shared.length === 0 ? (
                <Empty label="Nothing shared yet — set a model’s federation switch to “all” on the Models page." />
              ) : (
                <div>
                  {shared.map((m) => (
                    <span className="tag" key={m.id}>
                      {m.name}
                    </span>
                  ))}
                </div>
              )}
              {models.error && models.data && <Banner>Refresh failed: {models.error}</Banner>}
              <p className="dim">
                Only <code className="mono">all</code> is shared from a spoke;{' '}
                <code className="mono">allowlist</code> names rows of this
                instance’s own peer table, which a spoke does not have.
              </p>
            </section>
          )}

          {/* ---- Section 2: Servers (peer registry) — hub only ---- */}
          {curRole === 'hub' && (
            <section className="panel">
              <h3>Servers</h3>
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
                              <td className="mono">
                                {p.version || '—'}
                                {/* Small note (plan 2026-11-25-version-awareness):
                                    a connected peer running an older build than
                                    this hub — the peer's owner should update. */}
                                {p.online && p.version_state === 'older' && (
                                  <span className="dim" title={`${p.version} is older than this server`}>
                                    {' '}
                                    old
                                  </span>
                                )}
                              </td>
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
          )}

          {/* ---- Section 3: Ledger — hub and spoke (standalone has none) ---- */}
          {curRole !== 'standalone' && <Ledger stats={stats} win={win} currency={currency} />}
        </>
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

/** Spoke channel state card: connected pair of cards, a retry hint when
 *  down, an empty note before the first status arrives. A component with
 *  early returns instead of a nested ternary in the page body. */
function SpokeConnection({ status }: { status: FedSettings['spoke'] }) {
  if (!status) return <Empty label="No channel state yet." />;
  if (!status.connected) {
    return (
      <Banner kind="info">
        Not connected — the spoke keeps retrying with backoff; check the hub URL and
        token above, and that your token is still enabled on the hub.
      </Banner>
    );
  }
  return (
    <div className="cards">
      <Card
        label="Channel"
        accent
        value={
          <span className="badge ok">
            connected{status.hub_name ? ` → ${status.hub_name}` : ''}
          </span>
        }
      />
      <Card label="Hub version" value={status.hub_version || '—'} />
    </div>
  );
}

/**
 * The served/consumed ledger (section 3). The payload is role-shaped, so the
 * tables follow the server's role, not the draft select: a hub gets the
 * per-user fairness table and the per-server rollup; a spoke gets what it served
 * to the hub and what its own users consumed on federated models. A refresh
 * failure keeps the last good payload on screen (useLoad retains data on error).
 * The hub fairness panel distinguishes three states (ticket #13): ledger
 * rows, "nothing to attribute" (only unattributable local usage exists —
 * named, so the gap reads as design, not a bug), and "no data at all".
 */
function Ledger({
  stats,
  win,
  currency,
}: {
  stats: LoadState<FedAdminStats | null>;
  win: StatsWindow;
  currency: string;
}) {
  const winLabel = windowLabel(win);
  if (stats.loading && !stats.data) return <Loading />;
  if (!stats.data) {
    return <Banner>{stats.error ?? 'Could not load the federation ledger.'}</Banner>;
  }
  if (stats.data.role === 'hub') {
    return (
      <>
        <FairnessPanel
          users={stats.data.users}
          localOnly={stats.data.local_only}
          winLabel={winLabel}
          currency={currency}
          variant="hub"
        />

        <section className="panel">
          <h3>Per server ({winLabel})</h3>
          {stats.data.peers.length === 0 ? (
            <Empty label="No federation traffic in this window." />
          ) : (
            <>
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th rowSpan={2}>Server</th>
                      <th colSpan={4} className="num">Served</th>
                      <th colSpan={4} className="num">Consumed *</th>
                    </tr>
                    <tr>
                      <th className="num">Req</th>
                      <th className="num">In new</th>
                      <th className="num">Out</th>
                      <th className="num">Cost</th>
                      <th className="num">Req</th>
                      <th className="num">In new</th>
                      <th className="num">Out</th>
                      <th className="num">Cost *</th>
                    </tr>
                  </thead>
                  <tbody>
                    {stats.data.peers.map((p) => (
                      <tr key={p.peer_id}>
                        <td>{p.peer}</td>
                        <td className="num">{fmtInt(p.served.requests)}</td>
                        <td className="num">{fmtInt(inNewTokens(p.served))}</td>
                        <td className="num">{fmtInt(p.served.completion_tokens)}</td>
                        <td className="num money">{fmtMoney(p.served.cost_micro, currency)}</td>
                        <td className="num">{fmtInt(p.consumed.requests)}</td>
                        <td className="num">{fmtInt(inNewTokens(p.consumed))}</td>
                        <td className="num">{fmtInt(p.consumed.completion_tokens)}</td>
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
    );
  }
  if (stats.data.role === 'spoke') {
    return (
      <>
        <FairnessPanel
          users={stats.data.users}
          localOnly={stats.data.local_only}
          winLabel={winLabel}
          currency={currency}
          variant="spoke"
        />

        <section className="panel">
          <h3>Served to hub users ({winLabel})</h3>
          {stats.data.served_by_source.length === 0 ? (
            <Empty label="Nothing served to the hub in this window." />
          ) : (
            <CountsTable
              head="Hub user"
              rows={stats.data.served_by_source}
              keyOf={(s) => s.fed_user}
              labelOf={(s) => s.fed_user}
              currency={currency}
            />
          )}
        </section>

        <section className="panel">
          <h3>Consumed by local users on federated models ({winLabel})</h3>
          {stats.data.consumed.length === 0 ? (
            <Empty label="No local user spent on federated models in this window." />
          ) : (
            <CountsTable
              head="User"
              rows={stats.data.consumed}
              keyOf={(c) => c.user_id}
              labelOf={(c) => c.user}
              currency={currency}
            />
          )}
        </section>
      </>
    );
  }
  return (
    <section className="panel">
      <Empty label="This instance is standalone — no federation ledger." />
    </section>
  );
}

/** One served/consumed rollup table, shared by the spoke ledger panels (both
 *  payloads carry {label, counts} rows): requests / in-new / out / cost. */
function CountsTable<T extends { counts: FedCounts }>({
  head,
  rows,
  keyOf,
  labelOf,
  currency,
}: {
  head: string;
  rows: T[];
  keyOf: (r: T) => string | number;
  labelOf: (r: T) => string;
  currency: string;
}) {
  return (
    <div className="table-scroll">
      <table>
        <thead>
          <tr>
            <th>{head}</th>
            <th className="num">Requests</th>
            <th className="num">In new</th>
            <th className="num">Out</th>
            <th className="num">Cost</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={keyOf(r)}>
              <td className="mono">{labelOf(r)}</td>
              <td className="num">{fmtInt(r.counts.requests)}</td>
              <td className="num">{fmtInt(inNewTokens(r.counts))}</td>
              <td className="num">{fmtInt(r.counts.completion_tokens)}</td>
              <td className="num money">{fmtMoney(r.counts.cost_micro, currency)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
