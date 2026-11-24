import { useEffect, useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router-dom';
import { api, errMsg, FED_ROLES } from '../api';
import type { FedCounts, FedRole, FedSettings } from '../api';
import { fmtInt, fmtMoney, inNewTokens } from '../format';
import { useFedRole } from '../fedrole';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import { Banner, Card, Empty, Field, Loading, StatsWindowSelect } from '../components/ui';

/**
 * Federation control (admin): the instance role switch, the spoke's dial-in
 * config with a live connection status, and — per role — the follow-up panel:
 * hub points at the Servers registry, spoke shows what this box shares and
 * what it has served/consumed.
 */
export default function AdminFederationPage() {
  const toast = useToast();
  const fedRole = useFedRole();
  const settings = useLoad(() => api.fedSettings(), []);
  const currency = useLoad(() => api.settings(), []).data?.currency ?? '';
  const [role, setRole] = useState<FedRole | null>(null);
  const [hubUrl, setHubUrl] = useState<string | null>(null);
  const [hubToken, setHubToken] = useState<string | null>(null);
  const [reveal, setReveal] = useState(false);
  const [busy, setBusy] = useState(false);
  const [days, setDays] = useState(30);

  const cur = settings.data;
  const curRole = role ?? cur?.role ?? 'standalone';
  const curUrl = hubUrl ?? cur?.hub_url ?? '';
  const curToken = hubToken ?? cur?.hub_token ?? '';
  const dirty =
    curRole !== cur?.role || curUrl !== cur?.hub_url || curToken !== cur?.hub_token;

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

  // Spoke panels need the share list and the served/consumed ledger.
  const models = useLoad(
    () => (curRole === 'spoke' ? api.models().then((r) => r.models) : Promise.resolve([])),
    [curRole],
  );
  const stats = useLoad(
    () => (curRole === 'spoke' ? api.fedAdminStats(days) : Promise.resolve(null)),
    [curRole, days],
  );

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      const res = await api.putFedSettings(curRole, curUrl.trim(), curToken.trim());
      // Publish the role the server says it persisted: the sidebar menu is
      // keyed on the shared role, so it follows the save at once — no reload,
      // and no stale "Connect server" entry left pointing at a page that no
      // longer applies.
      fedRole.saved(res.role);
      toast.push('success', 'Federation settings saved.');
      setRole(null);
      setHubUrl(null);
      setHubToken(null);
      settings.reload();
    } catch (err: unknown) {
      toast.push('error', errMsg(err));
    } finally {
      setBusy(false);
    }
  };

  const spokeStatus = cur?.spoke;
  // A spoke without a usable dial-in target cannot connect: the URL must
  // be http(s) and the token non-empty (the server validates too, but the
  // Save button should not offer a guaranteed 400).
  const spokeConfigValid =
    curRole !== 'spoke' ||
    (/^https?:\/\/\S+$/i.test(curUrl.trim()) && curToken.trim() !== '');
  // The server ships fed_visibility='all' AND model-enabled AND backend-
  // enabled; the model payload carries the model flag only, so a model on
  // a disabled backend can linger in this list (residual over-count, the
  // hub-side share filter is the authority).
  const shared = (models.data ?? []).filter((m) => m.fed_visibility === 'all' && m.enabled);
  const spokeStats = stats.data && stats.data.role === 'spoke' ? stats.data : null;

  return (
    <div className="page">
      <div className="page-head">
        <h2>Federation</h2>
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
              (servers) connecting here; manage them on the{' '}
              <Link to="/admin/servers">Servers</Link> page. <b>spoke</b> — this
              box dials a hub and shares its opted-in models there.
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
                {!spokeConfigValid && (
                  <p className="dim">
                    Hub URL must start with http:// or https://, and the hub token is
                    required — Save stays disabled until both are filled.
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
              <div className="panel-head">
                <h3>Connection</h3>
                <StatsWindowSelect value={days} onChange={setDays} />
              </div>
              <SpokeConnection status={spokeStatus} />
            </section>
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

          {curRole === 'spoke' && (
            <section className="panel">
              <h3>Served to hub users (last {days} days)</h3>
              {stats.loading && !stats.data ? (
                <Loading />
              ) : stats.error && !stats.data ? (
                <Banner>Could not load the federation ledger: {stats.error}</Banner>
              ) : !spokeStats || spokeStats.served_by_source.length === 0 ? (
                <Empty label="Nothing served to the hub in this window." />
              ) : (
                <CountsTable
                  head="Hub user"
                  rows={spokeStats.served_by_source}
                  keyOf={(s) => s.fed_user}
                  labelOf={(s) => s.fed_user}
                  currency={currency}
                />
              )}
            </section>
          )}

          {curRole === 'spoke' && (
            <section className="panel">
              <h3>Consumed by local users on federated models (last {days} days)</h3>
              {stats.loading && !stats.data ? (
                <Loading />
              ) : stats.error && !stats.data ? (
                <Banner>Could not load the federation ledger: {stats.error}</Banner>
              ) : !spokeStats || spokeStats.consumed.length === 0 ? (
                <Empty label="No local user spent on federated models in this window." />
              ) : (
                <CountsTable
                  head="User"
                  rows={spokeStats.consumed}
                  keyOf={(c) => c.user_id}
                  labelOf={(c) => c.user}
                  currency={currency}
                />
              )}
            </section>
          )}

          {curRole === 'hub' && (
            <section className="panel">
              <h3>Hub</h3>
              <p className="dim">
                This relay accepts federation channels from other relays. Manage
                their server tokens, online state and the served/consumed ledger
                on the <Link to="/admin/servers">Servers</Link> page; users get
                their own token on <Link to="/keys">My keys</Link> and follow the{' '}
                <Link to="/setup">setup guide</Link> on their side.
              </p>
            </section>
          )}
        </>
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

/** One served/consumed rollup table, shared by the spoke panels (both
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
