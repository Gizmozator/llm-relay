import { useEffect, useMemo, useState } from 'react';
import { api, errMsg } from '../api';
import type { LiveResponse } from '../api';
import { useAuth } from '../auth';
import { fmtAgo, fmtDateTime, fmtDurationSec, fmtInt } from '../format';
import { Banner, Empty, Loading } from '../components/ui';
import LineChart from '../components/LineChart';

const POLL_MS = 5000;
const WINDOW_MIN = 60;

type Metric = 'requests' | 'tokens';

const METRIC_OPTIONS: { value: Metric; label: string }[] = [
  { value: 'requests', label: 'Requests / min' },
  { value: 'tokens', label: 'Tokens / min' },
];

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
  const [metric, setMetric] = useState<Metric>('requests');

  useEffect(() => {
    let stopped = false;
    const tick = () => {
      if (document.hidden) return;
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
  const onlineCount = (data?.models ?? []).filter((m) => m.online).length;
  const hourRequests = buckets.reduce((s, b) => s + b.requests, 0);

  const chart = useMemo(() => {
    return {
      labels: buckets.map((b) => b.minute.slice(11, 16)),
      values: buckets.map((b) =>
        metric === 'requests' ? b.requests : b.prompt_tokens + b.completion_tokens,
      ),
    };
  }, [buckets, metric]);

  const fmtValue = (v: number) => fmtInt(v);

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
          value={`${fmtInt(onlineCount)} / ${fmtInt(data?.models.length ?? 0)}`}
        />
        <Card label="Requests (hour)" value={fmtInt(hourRequests)} />
      </div>

      <section className="panel">
        <div className="panel-head">
          <h3>Live traffic</h3>
          <label className="field">
            <select
              className="input"
              value={metric}
              onChange={(e) => setMetric(e.target.value as Metric)}
            >
              {METRIC_OPTIONS.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
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
            label={METRIC_OPTIONS.find((o) => o.value === metric)?.label ?? ''}
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
            <Empty label="No active requests." />
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
                  {(data.connections ?? []).map((c, i) => (
                    <tr key={`${c.user}/${c.model}/${c.started_at}/${i}`}>
                      <td>{c.user}</td>
                      <td>{c.key}</td>
                      <td className="mono">
                        {c.model}
                        {c.streaming && (
                          <span className="badge ok" title="streaming response">
                            stream
                          </span>
                        )}
                      </td>
                      <td>{c.endpoint}</td>
                      <td className="mono" title={c.session}>
                        {c.session ? c.session.slice(0, 8) : '—'}
                      </td>
                      <td className="mono">{c.ip || '—'}</td>
                      <td className="num">{fmtDurationSec(c.seconds)}</td>
                    </tr>
                  ))}
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
                  <tr key={m.name}>
                    <td className="mono">{m.name}</td>
                    <td>{m.backend}</td>
                    <td>
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
                    </td>
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

function Card({ label, value, accent }: { label: string; value: string; accent?: boolean }) {
  return (
    <div className={`card${accent ? ' card-accent' : ''}`}>
      <div className="card-label">{label}</div>
      <div className="card-value">{value}</div>
    </div>
  );
}
