import { Link } from 'react-router-dom';
import { api } from '../api';
import { useAuth } from '../auth';
import { useLoad } from '../useLoad';
import { Banner, CodeBlock, CopyField, Loading } from '../components/ui';
// Canonical connector templates: the dashboard shows (and lets you copy) the
// very files that ship in deploy/remote-backend/, imported at build time so
// the page can never drift from them — edit the file, rebuild the web, it
// updates.
import composeRaw from '../../../deploy/remote-backend/docker-compose.yml?raw';
import connectorEnvRaw from '../../../deploy/remote-backend/.env.example?raw';
// The full-server pair (dashboard + admin), same build-time import, for a
// machine that has no relay yet and wants the whole thing rather than a
// bare connector.
import fullComposeRaw from '../../../deploy/full-server/docker-compose.yml?raw';
import fullEnvRaw from '../../../deploy/full-server/.env.example?raw';

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
  // The shipped .env.example carries a placeholder relay; when this hub
  // advertises its own URL (PUBLIC_RELAY_URL), fill it into the copyable env
  // so the file works as-is. Everything else is copied verbatim from the repo
  // file.
  const connectorEnv = hubUrl
    ? connectorEnvRaw.replace(/^RELAY_URL=.*$/m, () => `RELAY_URL=${hubUrl}`)
    : connectorEnvRaw;

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
          {info.data?.tls_fingerprint ? (
            <CopyField
              label="Hub pin (this hub uses a self-signed cert — paste as hub_pin)"
              value={info.data.tls_fingerprint}
              hint="No certificate configured."
            />
          ) : null}
        </div>
        <p className="dim">
          and paste your <code className="mono">fed-…</code> token
          {info.data?.tls_fingerprint
            ? ' and the hub pin above'
            : ''}
          . Save — the channel comes up within seconds; the page shows its live status.
        </p>
      </section>

      <section className="panel">
        <h3>3. Choose what to share</h3>
        <p className="dim">
          Nothing is shared until you opt in: on your relay’s{' '}
          <code className="mono">Admin → Models</code> page set each model’s{' '}
          <b>Share</b> to <code className="mono">all</code>. Paid or private
          models simply stay <code className="mono">local</code> and never
          appear on the hub. (The <code className="mono">allowlist</code> and{' '}
          <code className="mono">owner only</code> choices share with selected
          relays only — on a spoke they are effectively local: only{' '}
          <code className="mono">all</code> is pushed to the hub.)
        </p>
      </section>

      <section className="panel">
        <h3>No relay on that machine yet? Option A — full relay server</h3>
        <p className="dim">
          Steps 1–3 assume the machine already runs its own relay. If it does
          not and you want the full server — a dashboard, several models, your
          own users and keys — start it with these two files. Fill in{' '}
          <code className="mono">.env</code>, run{' '}
          <code className="mono">docker compose up -d</code>, open the dashboard
          on port 8081, then come back to steps 2–3 above to point it at this
          hub.
        </p>
        <CodeBlock title="docker-compose.yml" text={fullComposeRaw} />
        <CodeBlock title=".env" text={fullEnvRaw} />
      </section>

      <section className="panel">
        <h3>Option B — bare engine box (remote backend, no dashboard)</h3>
        <p className="dim">
          If that machine runs just one engine and you only want to expose it to{' '}
          <b>this</b> relay: have an admin create a{' '}
          <b>remote backend</b> on the <Link to="/admin/backends">Backends</Link>{' '}
          page — the <code className="mono">rb-…</code> connect token is shown
          once. Put these two files next to each other, fill in the engine and
          that token, then <code className="mono">docker compose up -d</code>.
          The box dials in over one outbound channel (no inbound ports), shows up
          as a backend here, and its models arrive via <b>Sync models</b>.
        </p>
        <p className="dim">
          The connector is the same <code className="mono">llm-relay</code>{' '}
          binary in connector mode (<code className="mono">HEADLESS=true</code>):
          it owns no state and serves no dashboard — everything it exposes
          belongs to its relay. By default it binds no ports at all; to let this
          box also act as its LAN’s gateway to the relay, uncomment{' '}
          <code className="mono">PROXY_LISTEN=8080</code> in the{' '}
          <code className="mono">.env</code> below — the connector then serves
          a local <code className="mono">/v1</code> listener and forwards
          clients over the channel (the relay authenticates each client’s{' '}
          <code className="mono">sk-</code> key itself — no keys live on the
          box). It is <b>not</b> a relay itself and cannot join the federation
          as a spoke; for that, use option A.
        </p>
        <CodeBlock title="docker-compose.yml" text={composeRaw} />
        <CodeBlock title=".env" text={connectorEnv} />
        {!hubUrl && (
          <p className="dim">
            The hub admin has not set <code className="mono">PUBLIC_RELAY_URL</code>,
            so <code className="mono">RELAY_URL</code> above is still the example
            host — replace it with this relay’s URL before starting.
          </p>
        )}
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
