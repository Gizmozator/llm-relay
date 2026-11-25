import { useEffect, useState } from 'react';
import { api } from '../api';
import type { AuditEntry } from '../api';
import { fmtDateTime } from '../format';
import { useLoad } from '../useLoad';
import { Banner, Empty, Loading } from '../components/ui';

const PAGE_SIZE = 50;

// Category filters mirror the action prefixes the server writes
// (plan 2026-11-25-audit-log-v1): "<category>.<verb>".
const CATEGORIES = [
  { value: '', label: 'Everything' },
  { value: 'user', label: 'User accounts' },
  { value: 'settings', label: 'Settings' },
  { value: 'federation', label: 'Federation config' },
  { value: 'backend', label: 'Backends' },
  { value: 'model', label: 'Models' },
  { value: 'group', label: 'Model groups' },
  { value: 'peer', label: 'Peer servers' },
  { value: 'tls', label: 'TLS certificate' },
  { value: 'fed', label: 'Federation channels' },
  { value: 'remote', label: 'Remote backends' },
];

/**
 * The audit log: who changed what, when. Append-only by design — the API
 * has no modify or delete route, and the actor is a username snapshot, so
 * a later rename or account deletion cannot rewrite history. Newest first,
 * paged, filterable by category / actor / free text.
 */
export default function AdminAuditPage() {
  const [category, setCategory] = useState('');
  const [actor, setActor] = useState('');
  const [q, setQ] = useState('');
  const [page, setPage] = useState(1);
  // Any filter change resets to the first page (a deep page of a new
  // filter result would open on nothing).
  useEffect(() => setPage(1), [category, actor, q]);

  const data = useLoad(
    () =>
      api.audit({
        category: category || undefined,
        actor: actor.trim() || undefined,
        q: q.trim() || undefined,
        page,
        page_size: PAGE_SIZE,
      }),
    [category, actor, q, page],
  );
  const totalPages = data.data ? Math.max(1, Math.ceil(data.data.total / PAGE_SIZE)) : 1;
  useEffect(() => {
    if (data.data && page > totalPages) setPage(totalPages);
  }, [data.data, page, totalPages]);

  return (
    <div className="page">
      <div className="page-head">
        <h2>Audit log</h2>
        <button className="btn small" onClick={() => data.reload()}>
          Refresh
        </button>
      </div>

      <section className="panel">
        <div className="form-row">
          <select className="input" value={category} onChange={(e) => setCategory(e.target.value)}
            aria-label="Category">
            {CATEGORIES.map((c) => (
              <option key={c.value} value={c.value}>
                {c.label}
              </option>
            ))}
          </select>
          <input
            className="input"
            placeholder="Actor (username)"
            value={actor}
            onChange={(e) => setActor(e.target.value)}
          />
          <input
            className="input"
            placeholder="Search target / detail…"
            value={q}
            onChange={(e) => setQ(e.target.value)}
          />
        </div>

        {data.loading && !data.data ? (
          <Loading />
        ) : data.error && !data.data ? (
          <Banner>{data.error}</Banner>
        ) : !data.data || data.data.entries.length === 0 ? (
          <Empty label="Nothing matches — the log only grows; every account, settings, backend, model or connection change lands here." />
        ) : (
          <>
            {data.error && <Banner>Refresh failed — showing the last loaded page. {data.error}</Banner>}
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Time</th>
                    <th>Actor</th>
                    <th>Action</th>
                    <th>Target</th>
                    <th>Detail</th>
                  </tr>
                </thead>
                <tbody>
                  {data.data.entries.map((e: AuditEntry) => (
                    <tr key={e.id}>
                      <td className="nowrap">{fmtDateTime(e.ts)}</td>
                      <td>{e.actor}</td>
                      <td className="mono">{e.action}</td>
                      <td>{e.target}</td>
                      <td className="dim">{e.detail}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <div className="pager">
              <button className="btn small" disabled={page <= 1} onClick={() => setPage((p) => p - 1)}>
                ← Prev
              </button>
              <span className="pager-info">
                page {data.data.page} / {totalPages} · {data.data.total} entries
              </span>
              <button
                className="btn small"
                disabled={page >= totalPages}
                onClick={() => setPage((p) => p + 1)}
              >
                Next →
              </button>
            </div>
          </>
        )}
        <p className="dim">
          Append-only: entries are never edited or removed, and the actor name is a snapshot taken
          at the time of the act — renaming or deleting an account cannot rewrite history.
        </p>
      </section>
    </div>
  );
}
