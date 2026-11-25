import { useState } from 'react';
import { api } from '../api';
import { useLoad } from '../useLoad';
import { FairnessPanel } from '../components/FairnessPanel';
import { fmtInt, fmtMoney, inNewTokens } from '../format';
import {
  Banner,
  Empty,
  Loading,
  StatsWindowSelect,
  windowLabel,
  windowToQuery,
} from '../components/ui';
import type { StatsWindow } from '../components/ui';

/**
 * Fairness (personal) — every user's own row of the federation ledger
 * (plan 2026-11-25-spoke-fairness): served (their hosted models and owned
 * backends serving other accounts) vs consumed (what they spent on hardware
 * owned by others — local cross-user and hub-hosted models alike). The
 * scoped endpoint answers for both roles and carries the display currency,
 * so this page needs nothing admin-only. Box-level views — the other users,
 * the Federation virtual account, who the box served — stay on the admin
 * Federation page: this page never sees them. On a spoke the caller also
 * gets their raw spending on federated models as a separate slice: the
 * fairness table folds it into consumed together with local cross-user
 * traffic, the slice isolates it.
 */
export default function FairnessPage() {
  const [win, setWin] = useState<StatsWindow>({ kind: 'days', days: 30 });
  const stats = useLoad(() => api.fedMyStats(windowToQuery(win)), [win]);
  const d = stats.data;
  const currency = d && d.role !== 'standalone' ? d.currency : '';
  const winLabel = windowLabel(win);

  return (
    <div className="page">
      <div className="page-head">
        <h2>My fairness</h2>
        <StatsWindowSelect value={win} onChange={setWin} />
      </div>
      {stats.loading && !d ? <Loading /> : null}
      {!d && !stats.loading ? (
        <Banner>{stats.error ?? 'Could not load your fairness ledger.'}</Banner>
      ) : null}
      {d?.role === 'standalone' && (
        <section className="panel">
          <Empty label="This instance is standalone — no federation, nothing to balance." />
        </section>
      )}
      {d && d.role !== 'standalone' && (
        <>
          <FairnessPanel
            users={d.totals}
            localOnly={d.local_only}
            winLabel={winLabel}
            currency={currency}
            variant="personal"
          />
          {d.role === 'spoke' && (
            <section className="panel">
              <h3>Your spending on federated models ({winLabel})</h3>
              {d.consumed.length === 0 ? (
                <Empty label="You spent nothing on federated models in this window." />
              ) : (
                <div className="table-scroll">
                  <table>
                    <thead>
                      <tr>
                        <th>Federated models</th>
                        <th className="num">Requests</th>
                        <th className="num">In new</th>
                        <th className="num">Out</th>
                        <th className="num">Cost</th>
                      </tr>
                    </thead>
                    <tbody>
                      {d.consumed.map((c) => (
                        <tr key={c.user_id}>
                          <td>{c.user}</td>
                          <td className="num">{fmtInt(c.counts.requests)}</td>
                          <td className="num">{fmtInt(inNewTokens(c.counts))}</td>
                          <td className="num">{fmtInt(c.counts.completion_tokens)}</td>
                          <td className="num money">{fmtMoney(c.counts.cost_micro, currency)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
              <p className="dim footnote">
                This is part of your <b>Consumed</b> column above — the fairness table adds it to
                what you spent on other local accounts’ hardware.
              </p>
            </section>
          )}
        </>
      )}
    </div>
  );
}
