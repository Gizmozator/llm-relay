import { Link } from 'react-router-dom';
import { api } from '../api';
import { useAuth } from '../auth';
import { useLoad } from '../useLoad';
import { Banner, CopyField, Loading } from '../components/ui';

/**
 * Setup guide ("set up your home server"): how a user enrolls their own
 * relay as a spoke of this hub. Hub-only surface — rendered only while this
 * instance is a hub (the AgentGuide gating pattern, via GET /api/federation/role).
 */
export default function SetupGuidePage() {
  const { user } = useAuth();
  const role = useLoad(() => api.fedRole(), []);
  const info = useLoad(() => api.info(), []);
  // The spoke dials {hub_url}/federation/v1/connect, so what goes into its
  // config is this relay's base URL — no path.
  const hubUrl = (info.data?.relay_url ?? '').replace(/\/+$/, '');

  if (role.loading && !role.data) return <Loading />;
  if (role.error) return <Banner>Could not load federation role: {role.error}</Banner>;
  if (role.data?.role !== 'hub') {
    return (
      <div className="page">
        <div className="page-head">
          <h2>Connect your server</h2>
        </div>
        <Banner kind="info">
          This relay is not a federation hub right now, so there is nothing to
          connect to.{' '}
          {user?.role === 'admin' ? (
            <>
              An admin can enable the hub role on the{' '}
              <Link to="/admin/federation">Federation</Link> page.
            </>
          ) : (
            'An admin can enable the hub role in the admin Federation settings.'
          )}
        </Banner>
      </div>
    );
  }

  return (
    <div className="page">
      <div className="page-head">
        <h2>Connect your server</h2>
        <span className="dim">share your machine’s models with the group</span>
      </div>

      <section className="panel">
        <h3>What this does</h3>
        <p className="dim">
          Your relay joins this hub as a <b>spoke</b>: it keeps running on its
          own, and the models you opt in become visible (and usable) for the
          group through the hub. Your spoke dials the hub out over one
          encrypted channel — the hub never needs to reach into your network.
        </p>
      </section>

      <section className="panel">
        <h3>1. Create your server token</h3>
        <p className="dim">
          On <b>this</b> dashboard, open <Link to="/keys">My keys</Link> and
          create a credential of type <b>Server token</b>, named after your
          machine. The <code className="mono">fed-…</code> token is shown once —
          copy it now.
        </p>
      </section>

      <section className="panel">
        <h3>2. Point your relay at this hub</h3>
        <p className="dim">
          On <b>your own</b> relay’s dashboard, open{' '}
          <code className="mono">Admin → Federation</code>, set the role to{' '}
          <b>spoke</b>, and fill in:
        </p>
        <div className="form-stack">
          {/* Gate on the load: while info is in flight (or failed) hubUrl is
              empty, and the CopyField hint would falsely claim the hub
              admin left PUBLIC_RELAY_URL unset. */}
          {info.loading && !info.data ? (
            <Loading label="Loading hub URL…" />
          ) : info.error ? (
            <Banner>Could not load this relay’s URL: {info.error}</Banner>
          ) : (
            <CopyField
              label="Hub URL"
              value={hubUrl}
              hint="Not configured by the hub admin (PUBLIC_RELAY_URL) — ask the admin for the hub’s base URL."
            />
          )}
        </div>
        <p className="dim">
          and paste your <code className="mono">fed-…</code> token. Save — the
          channel comes up within seconds; the page shows its live status.
        </p>
      </section>

      <section className="panel">
        <h3>3. Choose what to share</h3>
        <p className="dim">
          Nothing is shared until you opt in: on your relay’s{' '}
          <code className="mono">Admin → Models</code> page set each model’s
          federation switch to <code className="mono">all</code>. Paid or
          private backends simply stay <code className="mono">off</code> and
          never appear on the hub.
        </p>
      </section>

      <section className="panel">
        <h3>Good to know</h3>
        <p className="dim">
          Usage through your models is metered to the hub user who asked, and
          credited to your account as served capacity — the hub’s Federation
          page keeps the fairness ledger. If your spoke goes offline, its models
          vanish from the hub until it reconnects; the token can be rotated (or
          revoked) at any time from My keys.
        </p>
      </section>
    </div>
  );
}
