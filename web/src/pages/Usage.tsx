import { useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../api';
import type { AdminUser, GroupBy, UsageSummary } from '../api';
import { daysAgoISO, fmtDateTime, fmtInt, fmtMoney, inNewTokens } from '../format';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import { Banner, Card, Empty, Loading } from '../components/ui';
import LineChart from '../components/LineChart';

const GROUP_OPTIONS: { value: GroupBy; label: string }[] = [
  { value: 'day', label: 'Day' },
  { value: 'user', label: 'User' },
  { value: 'key', label: 'Key' },
  { value: 'model', label: 'Model' },
  { value: 'ip', label: 'Client IP' },
  { value: 'endpoint', label: 'Endpoint' },
  { value: 'session', label: 'Session' },
];

const PAGE_SIZE = 25;

export default function UsagePage({ admin }: { admin: boolean }) {
  const toast = useToast();
  const [from, setFrom] = useState(daysAgoISO(29));
  const [to, setTo] = useState(daysAgoISO(0));
  const [groupBy, setGroupBy] = useState<GroupBy>(admin ? 'user' : 'day');
  const [userId, setUserId] = useState<number | ''>('');
  const [modelFilter, setModelFilter] = useState('');
  const [page, setPage] = useState(1);

  useEffect(() => setPage(1), [from, to, userId, groupBy, modelFilter]);

  const users = useLoad(
    () => (admin ? api.users().then((r) => r.users) : Promise.resolve([] as AdminUser[])),
    [admin],
  );

  const baseQuery = {
    from,
    to,
    user_id: userId === '' ? undefined : userId,
    model: modelFilter || undefined,
  };

  // Dropdown options: exactly the models used in range. Always fetched WITHOUT
  // the model filter so the list stays switchable while a model is selected.
  const modelOptions = useLoad(
    () =>
      api
        .usageSummary(admin, {
          from,
          to,
          user_id: userId === '' ? undefined : userId,
          group_by: 'model',
        })
        .then((r) => r.groups.map((g) => g.key)),
    [admin, from, to, userId],
  );

  const modelChoices = useMemo(() => {
    const keys = modelOptions.data ?? [];
    if (modelFilter && !keys.includes(modelFilter)) return [modelFilter, ...keys];
    return keys;
  }, [modelOptions.data, modelFilter]);

  // Day series drives the totals cards + the cost chart.
  const daySummary = useLoad(
    () => api.usageSummary(admin, { ...baseQuery, group_by: 'day' }),
    [admin, from, to, userId, modelFilter],
  );

  // Selected grouping drives the breakdown table (skipped when it is "day").
  const groupSummary = useLoad(
    () =>
      groupBy === 'day'
        ? Promise.resolve(null as UsageSummary | null)
        : api.usageSummary(admin, { ...baseQuery, group_by: groupBy }),
    [admin, from, to, userId, groupBy, modelFilter],
  );

  const detail = useLoad(
    () => api.usageDetail(admin, { ...baseQuery, page, page_size: PAGE_SIZE }),
    [admin, from, to, userId, modelFilter, page],
  );

  const currency = daySummary.data?.currency ?? '';
  const totals = daySummary.data?.totals;
  const tableGroups = useMemo(() => {
    const src = groupBy === 'day' ? daySummary.data?.groups : groupSummary.data?.groups;
    if (!src) return [];
    const copy = [...src];
    if (groupBy === 'day') copy.sort((a, b) => a.key.localeCompare(b.key));
    else copy.sort((a, b) => b.cost_micro - a.cost_micro);
    return copy;
  }, [groupBy, daySummary.data, groupSummary.data]);

  const chart = useMemo(() => {
    const groups = [...(daySummary.data?.groups ?? [])].sort((a, b) =>
      a.key.localeCompare(b.key),
    );
    return {
      labels: groups.map((g) => g.label),
      values: groups.map((g) => g.cost_micro / 1e6),
    };
  }, [daySummary.data]);

  const money = (micro: number) => fmtMoney(micro, currency);
  const fmtAxis = (v: number) => fmtMoney(v * 1e6, currency);

  // Surface a load failure as a toast too (the inline banner already shows it),
  // once per message while it persists. The set is per-mount and resets when
  // the error clears, so a later failure toasts again.
  const loadError = daySummary.error || groupSummary.error || detail.error;
  const push = toast.push;
  const seen = useRef(new Set<string>());
  useEffect(() => {
    if (!loadError) {
      seen.current.clear();
      return;
    }
    if (seen.current.has(loadError)) return;
    seen.current.add(loadError);
    push('error', loadError);
  }, [loadError, push]);

  const totalPages = detail.data ? Math.max(1, Math.ceil(detail.data.total / PAGE_SIZE)) : 1;

  return (
    <div className="page">
      <div className="page-head">
        <h2>{admin ? 'Usage — all users' : 'Usage'}</h2>
      </div>

      <div className="filter-bar">
        <label className="field">
          <span className="field-label">From</span>
          <input className="input" type="date" value={from} onChange={(e) => setFrom(e.target.value)} />
        </label>
        <label className="field">
          <span className="field-label">To</span>
          <input className="input" type="date" value={to} onChange={(e) => setTo(e.target.value)} />
        </label>
        {admin && (
          <label className="field">
            <span className="field-label">User</span>
            <select
              className="input"
              value={userId === '' ? '' : String(userId)}
              onChange={(e) => setUserId(e.target.value === '' ? '' : Number(e.target.value))}
            >
              <option value="">All users</option>
              {(users.data ?? []).map((u) => (
                <option key={u.id} value={u.id}>
                  {u.username}
                  {u.disabled ? ' (disabled)' : ''}
                </option>
              ))}
            </select>
          </label>
        )}
        <label className="field">
          <span className="field-label">Model</span>
          <select
            className="input"
            value={modelFilter}
            onChange={(e) => setModelFilter(e.target.value)}
          >
            <option value="">All models</option>
            {modelChoices.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span className="field-label">Group by</span>
          <select className="input" value={groupBy} onChange={(e) => setGroupBy(e.target.value as GroupBy)}>
            {GROUP_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </label>
      </div>

      {daySummary.loading && !daySummary.data ? (
        <Loading />
      ) : daySummary.error ? (
        <Banner>{daySummary.error}</Banner>
      ) : (
        <>
          <div className="cards">
            <Card label="Requests" value={fmtInt(totals?.requests ?? 0)} />
            <Card label="In new tokens" value={fmtInt(totals ? inNewTokens(totals) : 0)} />
            <Card label="Cached tokens" value={fmtInt(totals?.cached_tokens ?? 0)} />
            <Card label="Cache-write tokens" value={fmtInt(totals?.cache_write_tokens ?? 0)} />
            <Card label="Out tokens" value={fmtInt(totals?.completion_tokens ?? 0)} />
            <Card label={`Cost${currency ? ` (${currency})` : ''}`} value={money(totals?.cost_micro ?? 0)} accent />
          </div>

          <section className="panel">
            <h3>Cost by day</h3>
            {chart.labels.length === 0 ? (
              <Empty label="No usage in this range." />
            ) : (
              <LineChart labels={chart.labels} values={chart.values} label="Cost" format={fmtAxis} />
            )}
          </section>

          <section className="panel">
            <h3>Breakdown by {GROUP_OPTIONS.find((o) => o.value === groupBy)?.label.toLowerCase()}</h3>
            {groupSummary.loading && !groupSummary.data ? (
              <Loading />
            ) : groupSummary.error ? (
              <Banner>{groupSummary.error}</Banner>
            ) : tableGroups.length === 0 ? (
              <Empty label="No usage in this range." />
            ) : (
              <div className="table-scroll">
                <table>
                  <thead>
                    <tr>
                      <th>{groupByLabel(groupBy)}</th>
                      <th className="num">Requests</th>
                      <th className="num">In new</th>
                      <th className="num">Cached</th>
                      <th className="num">Cache write</th>
                      <th className="num">Out</th>
                      <th className="num">Cost</th>
                    </tr>
                  </thead>
                  <tbody>
                    {tableGroups.map((g) => (
                      <tr key={g.key || '(none)'}>
                        <td>{g.label || g.key || '(none)'}</td>
                        <td className="num">{fmtInt(g.requests)}</td>
                        <td className="num">{fmtInt(inNewTokens(g))}</td>
                        <td className="num">{fmtInt(g.cached_tokens)}</td>
                        <td className="num">{fmtInt(g.cache_write_tokens)}</td>
                        <td className="num">{fmtInt(g.completion_tokens)}</td>
                        <td className="num money">{money(g.cost_micro)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </>
      )}

      <section className="panel">
        <div className="panel-head">
          <h3>Recent requests</h3>
          {detail.data && (
            <div className="pager">
              <button
                className="btn small"
                disabled={page <= 1}
                onClick={() => setPage((p) => Math.max(1, p - 1))}
              >
                ← Prev
              </button>
              <span className="pager-info">
                {page} / {totalPages} · {fmtInt(detail.data.total)} rows
              </span>
              <button
                className="btn small"
                disabled={page >= totalPages}
                onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
              >
                Next →
              </button>
            </div>
          )}
        </div>
        {detail.loading && !detail.data ? (
          <Loading />
        ) : detail.error ? (
          <Banner>{detail.error}</Banner>
        ) : !detail.data || detail.data.rows.length === 0 ? (
          <Empty label="No requests in this range." />
        ) : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Time</th>
                  {admin && <th>User</th>}
                  <th>Key</th>
                  <th>Model</th>
                  <th>Endpoint</th>
                  <th>IP</th>
                  <th>Session</th>
                  <th className="num">In new</th>
                  <th className="num">Cached</th>
                  <th className="num">Cache write</th>
                  <th className="num">Out</th>
                  <th className="num">Cost</th>
                  <th className="num">Status</th>
                  <th className="num">Latency</th>
                  <th>Flags</th>
                </tr>
              </thead>
              <tbody>
                {detail.data.rows.map((r) => (
                  <tr key={r.id}>
                    <td className="nowrap">{fmtDateTime(r.ts)}</td>
                    {admin && <td>{r.username ?? '—'}</td>}
                    <td>{r.key_name}</td>
                    <td className="mono">
                      {r.model_requested}
                      {admin && r.model_upstream && r.model_upstream !== r.model_requested && (
                        <div className="dim" title="upstream model id the engine served (differs from the requested name when the relay maps public names to engine-specific ids)">
                          {r.model_upstream}
                        </div>
                      )}
                    </td>
                    <td>{r.endpoint}</td>
                    <td className="mono">{r.client_ip}</td>
                    <td className="mono dim" title={r.session_id ?? undefined}>
                      {r.session_id ? r.session_id.slice(0, 8) : '—'}
                    </td>
                    <td className="num">{fmtInt(inNewTokens(r))}</td>
                    <td className="num">{fmtInt(r.cached_tokens)}</td>
                    <td className="num">{fmtInt(r.cache_write_tokens)}</td>
                    <td className="num">{fmtInt(r.completion_tokens)}</td>
                    <td className="num money">{money(r.cost_micro)}</td>
                    <td className={`num ${r.status_code >= 400 ? 'text-red' : ''}`}>{r.status_code}</td>
                    <td className="num">{fmtInt(r.latency_ms)} ms</td>
                    <td className="flags">
                      {r.streamed && <span className="tag">stream</span>}
                      {r.usage_estimated && <span className="tag warn" title="Usage estimated, not reported by upstream">est.</span>}
                      {r.error && (
                        <span className="tag err" title={r.error}>
                          error
                        </span>
                      )}
                    </td>
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

function groupByLabel(g: GroupBy): string {
  switch (g) {
    case 'day':
      return 'Day';
    case 'user':
      return 'User';
    case 'key':
      return 'Key';
    case 'model':
      return 'Model';
    case 'ip':
      return 'Client IP';
    case 'endpoint':
      return 'Endpoint';
    case 'session':
      return 'Session';
  }
}
