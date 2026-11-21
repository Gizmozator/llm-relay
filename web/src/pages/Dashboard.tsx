import { useEffect, useMemo, useState } from 'react';
import { api, errMsg } from '../api';
import type { LiveResponse } from '../api';
import { useAuth } from '../auth';
import { fmtInt, fmtMoney } from '../format';
import { Banner, Empty, Loading } from '../components/ui';
import LineChart from '../components/LineChart';

const POLL_MS = 5000;
const WINDOW_MIN = 60;

type Metric = 'requests' | 'tokens' | 'cost';

const METRIC_OPTIONS: { value: Metric; label: string }[] = [
  { value: 'requests', label: 'Requests / min' },
  { value: 'tokens', label: 'Tokens / min' },
  { value: 'cost', label: 'Cost / min' },
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

  const currency = data?.currency ?? '';
  const buckets = data?.buckets ?? [];

  const activeTotal = useMemo(
    () => (data?.active ?? []).reduce((s, a) => s + a.count, 0),
    [data],
  );
  const onlineCount = (data?.models ?? []).filter((m) => m.online).length;
  const hourRequests = buckets.reduce((s, b) => s + b.requests, 0);
  const hourCost = buckets.reduce((s, b) => s + b.cost_micro, 0);

  const chart = useMemo(() => {
    return {
      labels: buckets.map((b) => b.minute.slice(11, 16)),
      values: buckets.map((b) =>
        metric === 'requests'
          ? b.requests
          : metric === 'tokens'
            ? b.prompt_tokens + b.completion_tokens
            : b.cost_micro / 1e6,
      ),
    };
  }, [buckets, metric]);

  const fmtValue =
    metric === 'cost'
      ? (v: number) => fmtMoney(v * 1e6, currency)
      : (v: number) => fmtInt(v);

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
        <Card label={`Cost (hour)${currency ? ` (${currency})` : ''}`} value={fmtMoney(hourCost, currency)} accent />
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
