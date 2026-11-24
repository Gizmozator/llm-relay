import { useEffect, useMemo, useState } from 'react';
import { api, errMsg } from '../api';
import type { LiveConnection, LiveModel, LiveResponse } from '../api';
import { useAuth } from '../auth';
import { fmtAgo, fmtDateTime, fmtDurationSec, fmtHourMinuteUTC, fmtInt } from '../format';
import { formatMetricValue, LIVE_METRICS } from '../metrics';
import type { Metric } from '../metrics';
import { Banner, Card, Empty, Loading } from '../components/ui';
import LineChart from '../components/LineChart';

const POLL_MS = 5000;
const WINDOW_MIN = 60;

/** Status badges for a real (per-row) model: online/offline, plus draining
 *  while online (a draining backend still serves running conversations). */
function memberStatus(m: LiveModel) {
  return (
    <>
      {m.online ? (
        <span className="badge ok">online</span>
      ) : (
        <span className="badge err">offline</span>
      )}
      {m.online && m.draining && (
        <span className="badge warn" title="No new sessions; running conversations finish">
          draining
        </span>
      )}
    </>
  );
}

// Shared explanation for the group rows (status cell + backend cell titles).
const POOL_HINT = 'Model group: several machines behind this name';

// Composite key for "rows sharing this name AND upstream" (NUL cannot
// occur in model names, so the parts cannot alias across fields).
const keyTwin = (name: string, upstream: string) => `${name}\u0000${upstream}`;

/** Pool row status cell: the server sends no member status on purpose
 *  (wiring is admin-only) — claiming "offline" would be a lie, so the cell
 *  shows a bare em-dash. */
function poolStatus() {
  return (
    <span className="dim" title={POOL_HINT}>
      —
    </span>
  );
}

/** Group connection rows per session (user+key+model+endpoint+session+IP).
 *  One busy agent session is many short requests, but the panel answers
 *  "who is on which model" — that is one row per group. Rows arrive sorted
 *  by start, so insertion order keeps the oldest group first. */
function groupConnections(rows: LiveConnection[]): LiveConnection[][] {
  const map = new Map<string, LiveConnection[]>();
  for (const c of rows) {
    // NUL separator (see keyTwin): usernames, key names and session ids are
    // free text — a '|' join could alias across fields ("a|b" + "c" vs
    // "a" + "b|c") and merge two different sessions into one row.
    const k = [c.user, c.key, c.model, c.endpoint, c.session ?? '', c.ip ?? ''].join('\u0000');
    const g = map.get(k);
    if (g) g.push(c);
    else map.set(k, [c]);
  }
  return [...map.values()];
}

/**
 * Default landing view: what the relay is doing right now. Polls /api/live
 * every 5 s while the tab is visible; the server caches backend health for
 * a minute, so the fast poll costs no upstream probes.
 */
export default function DashboardPage() {
  const { user } = useAuth();
  const admin = user?.role === 'admin';
  const [data, setData] = useState<LiveResponse | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [metric, setMetric] = useState<Metric>(LIVE_METRICS[0]);

  useEffect(() => {
    let stopped = false;
    // One request at a time: a slow /api/live (large catalog, cold cache)
    // must not let ticks pile up — overlapping responses can land out of
    // order and flash stale data.
    let inflight = false;
    const tick = () => {
      if (document.hidden || inflight) return;
      inflight = true;
      api
        .live(Boolean(admin), WINDOW_MIN)
        .then((d) => {
          if (!stopped) {
            setData(d);
            setError(null);
          }
        })
        .catch((e: unknown) => {
          if (!stopped) setError(errMsg(e));
        })
        .finally(() => {
          inflight = false;
        });
    };
    tick();
    const id = setInterval(tick, POLL_MS);
    const onVisible = () => {
      if (!document.hidden) tick();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      stopped = true;
      clearInterval(id);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [admin]);

  const buckets = data?.buckets ?? [];

  const activeTotal = useMemo(
    () => (data?.active ?? []).reduce((s, a) => s + a.count, 0),
    [data],
  );
  // Pool rows (explicit pool marker) carry no per-row online status on
  // purpose (member status is admin-only), so they are excluded from the
  // online count entirely — counting them in the denominator would claim
  // a group-only catalog is fully offline.
  const realModels = (data?.models ?? []).filter((m) => !m.pool);
  const onlineCount = realModels.filter((m) => m.online).length;
  // Group rows are counted separately (a dim suffix), not hidden: the card
  // must not under-report against a table that lists them too.
  const groupCount = (data?.models ?? []).length - realModels.length;
  // Twin discriminator (admins): only names carried by MORE THAN ONE row are
  // ambiguous, so only those get a dim suffix. Upstream is used only when it
  // is UNIQUE among that name's rows — canonical replicas share name AND
  // upstream, and for those only the row id distinguishes.
  const nameRowCounts = useMemo(() => {
    const counts = new Map<string, number>();
    const upstreamCounts = new Map<string, number>();
    for (const m of data?.models ?? []) {
      counts.set(m.name, (counts.get(m.name) ?? 0) + 1);
      if (m.upstream) {
        const k = keyTwin(m.name, m.upstream);
        upstreamCounts.set(k, (upstreamCounts.get(k) ?? 0) + 1);
      }
    }
    return { counts, upstreamCounts };
  }, [data]);
  const hourRequests = buckets.reduce((s, b) => s + b.requests, 0);

  const chart = useMemo(() => {
    return {
      labels: buckets.map((b) => fmtHourMinuteUTC(b.minute)),
      values: buckets.map((b) => metric.value(b)),
    };
  }, [buckets, metric]);

  // Live buckets carry no cost, so every live metric is a plain count; the
  // empty currency is never rendered (no money metric reaches this page).
  const fmtValue = (v: number) => formatMetricValue(metric, v, '');

  // Utilization graph: one line per model with traffic in the window. The
  // axis tops out at the widest concurrency_limit × 100 % (4 slots → 400 %),
  // auto-scaling when no model has a limit set.
  const utilSeries = useMemo(
    () => (data?.util ?? []).map((u) => ({ label: u.model, values: u.values })),
    [data],
  );
  const utilYMax = useMemo(() => {
    const limits = (data?.util ?? [])
      .map((u) => u.concurrency_limit * 100)
      .filter((v) => v > 0);
    return limits.length ? Math.max(...limits) : undefined;
  }, [data]);

  if (!data && !error) return <Loading label="Loading live data…" />;

  return (
    <div className="page">
      <div className="page-head">
        <h2>Dashboard</h2>
        <span className="dim">
          last {WINDOW_MIN} min · refreshes every {POLL_MS / 1000} s
        </span>
      </div>

      {error && (
        <Banner>
          {error}{data ? ' — showing the last successful update.' : ''}
        </Banner>
      )}

      <div className="cards">
        <Card label="Active streams" value={fmtInt(activeTotal)} accent={activeTotal > 0} />
        <Card
          label="Models online"
          title={
            groupCount > 0
              ? 'Model groups have no per-row online status (member wiring is admin-only), so they are counted separately'
              : undefined
          }
          value={
            <>
              {realModels.length > 0 ? `${fmtInt(onlineCount)} / ${fmtInt(realModels.length)}` : '—'}
              {groupCount > 0 && (
                <span className="dim card-value-sub">
                  {' '}
                  +{fmtInt(groupCount)} group{groupCount === 1 ? '' : 's'}
                </span>
              )}
            </>
          }
        />
        <Card label="Requests (hour)" value={fmtInt(hourRequests)} />
      </div>

      <section className="panel">
        <div className="panel-head">
          <h3>Live traffic</h3>
          <label className="field">
            <select
              className="input"
              value={metric.id}
              onChange={(e) => {
                const next = LIVE_METRICS.find((m) => m.id === e.target.value);
                if (next) setMetric(next);
              }}
            >
              {LIVE_METRICS.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.label}
                </option>
              ))}
            </select>
          </label>
        </div>
        {buckets.length === 0 ? (
          <Empty label="No data yet." />
        ) : (
          <LineChart
            labels={chart.labels}
            values={chart.values}
            label={metric.label}
            format={fmtValue}
          />
        )}
      </section>

      <section className="panel">
        <div className="panel-head">
          <h3>Utilization by model</h3>
          <span className="dim">
            {utilYMax
              ? `busy time per minute · axis max ${utilYMax} % = ${utilYMax / 100} parallel streams`
              : 'busy time per minute · over 100 % means parallel streams'}
          </span>
        </div>
        {utilSeries.length === 0 ? (
          <Empty label="No requests in this window." />
        ) : (
          <LineChart
            labels={chart.labels}
            values={[]}
            label=""
            format={(v) => `${Math.round(v)}%`}
            series={utilSeries}
            legend
            yMax={utilYMax}
          />
        )}
      </section>

      {admin && (
        <section className="panel">
          <div className="panel-head">
            <h3>Active connections</h3>
            <span className="dim">who is on which model right now</span>
          </div>
          {!data || (data.connections ?? []).length === 0 ? (
            <Empty label="No requests in the last minute." />
          ) : (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>User</th>
                    <th>Key</th>
                    <th>Model</th>
                    <th>Endpoint</th>
                    <th>Session</th>
                    <th>IP</th>
                    <th className="num">Running</th>
                  </tr>
                </thead>
                <tbody>
                  {groupConnections(data.connections ?? []).map((rows, i) => {
                    const first = rows[0];
                    const running = rows.filter((c) => c.running);
                    const done = rows.length - running.length;
                    let cell: string;
                    if (running.length > 0) {
                      const secs = Math.max(...running.map((c) => c.seconds));
                      cell =
                        fmtDurationSec(secs) +
                        (running.length > 1 ? ` ×${running.length}` : '') +
                        (done > 0 ? ` · ${done} done` : '');
                    } else {
                      const latest = rows.reduce((a, c) =>
                        (c.ended_at ?? '') >= (a.ended_at ?? '') ? c : a,
                      );
                      cell = `done ${fmtAgo(latest.ended_at)}` + (rows.length > 1 ? ` ×${rows.length}` : '');
                    }
                    return (
                      <tr
                        key={`${first.user}/${first.model}/${first.started_at}/${i}`}
                        className={running.length > 0 ? undefined : 'row-muted'}
                      >
                        <td>{first.user}</td>
                        <td>{first.key}</td>
                        <td className="mono">
                          {first.model}
                          {first.streaming && (
                            <span className="badge ok" title="streaming response">
                              stream
                            </span>
                          )}
                        </td>
                        <td>{first.endpoint}</td>
                        <td className="mono" title={first.session}>
                          {first.session ? first.session.slice(0, 8) : '—'}
                        </td>
                        <td className="mono">{first.ip || '—'}</td>
                        <td className="num" title={`${running.length} running · ${done} finished in the last minute`}>
                          {cell}
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

      {admin && (
        <section className="panel">
          <div className="panel-head">
            <h3>Last used</h3>
            <span className="dim">most recent request per user (24 h)</span>
          </div>
          {!data || (data.last_used ?? []).length === 0 ? (
            <Empty label="No usage in the last 24 hours." />
          ) : (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>User</th>
                    <th>Last used</th>
                  </tr>
                </thead>
                <tbody>
                  {(data.last_used ?? []).map((u) => (
                    <tr key={u.user}>
                      <td>{u.user}</td>
                      <td title={fmtDateTime(u.last_used)}>{fmtAgo(u.last_used)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      )}

      <section className="panel">
        <h3>Models</h3>
        {!data || data.models.length === 0 ? (
          <Empty label="No models in the catalog." />
        ) : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Model</th>
                  <th>Backend</th>
                  <th>Status</th>
                  <th className="num">Active</th>
                </tr>
              </thead>
              <tbody>
                {data.models.map((m) => (
                  // Keyed by id, not name: group members share a name by
                  // design, and each row is one physical member with its
                  // own (per-member) active count. For users a group is
                  // one synthetic pool row (empty backend).
                  <tr key={m.id}>
                    <td className="mono">
                      {m.name}
                      {admin && (nameRowCounts.counts.get(m.name) ?? 0) > 1 && (
                        // Twin discriminator: upstream only when it is unique
                        // among this name's rows (canonical replicas share it),
                        // else the row id — unique by construction.
                        <span className="dim">
                          {' · '}
                          {m.upstream &&
                          m.upstream !== m.name &&
                          (nameRowCounts.upstreamCounts.get(keyTwin(m.name, m.upstream)) ?? 0) === 1
                            ? m.upstream
                            : `#${m.id}`}
                        </span>
                      )}
                    </td>
                    <td>
                      {m.pool ? (
                        <span className="dim" title={POOL_HINT}>
                          group
                        </span>
                      ) : (
                        m.backend
                      )}
                    </td>
                    <td>{m.pool ? poolStatus() : memberStatus(m)}</td>
                    <td className="num">{fmtInt(m.active)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
