import type { FedCounts, FedLocalOnlyEntry, FedUserTotals } from '../api';
import { fmtInt, fmtMoney, inNewTokens } from '../format';
import { fedLedgerState, localOnlyLabel } from '../pages/fedLedger';
import { Empty } from './ui';

/** Sign class for a served−consumed delta: green when the row gave more than
 *  it took (net provider), red when it took more (net consumer), plain at 0. */
function netClass(n: number): string {
  return n > 0 ? 'net-pos' : n < 0 ? 'net-neg' : '';
}

/** The served−consumed delta across the same four columns as the Served and
 *  Consumed groups, so the fairness balance reads at a glance instead of
 *  being subtracted by hand. Positive = this account's hardware served more
 *  than it consumed (a net contributor to the federation). */
function NetCells({
  served,
  consumed,
  currency,
}: {
  served: FedCounts;
  consumed: FedCounts;
  currency: string;
}) {
  const req = served.requests - consumed.requests;
  const inNew = inNewTokens(served) - inNewTokens(consumed);
  const out = served.completion_tokens - consumed.completion_tokens;
  const cost = served.cost_micro - consumed.cost_micro;
  return (
    <>
      <td className={`num ${netClass(req)}`}>{fmtInt(req)}</td>
      <td className={`num ${netClass(inNew)}`}>{fmtInt(inNew)}</td>
      <td className={`num ${netClass(out)}`}>{fmtInt(out)}</td>
      <td className={`num money ${netClass(cost)}`}>{fmtMoney(cost, currency)}</td>
    </>
  );
}

/**
 * The fairness table shared by the hub admin view, the spoke admin view and
 * the personal view every user sees (plans 2026-11-25-fairness-ledger and
 * 2026-11-25-spoke-fairness): served/consumed/Net per account with the
 * three honest empty states (ticket #13): ledger rows → the table; no rows
 * but unattributable accounts → "nothing to attribute, usage is on the
 * Usage page"; neither → no data. The spoke copy explains the Federation
 * virtual account — the remote side as seen from this box, mirroring the
 * box's owner account on the hub. The personal variant renders the caller's
 * own single row from the scoped endpoint.
 */
export function FairnessPanel({
  users,
  localOnly,
  winLabel,
  currency,
  variant,
}: {
  users: FedUserTotals[];
  localOnly: FedLocalOnlyEntry[];
  winLabel: string;
  currency: string;
  variant: 'hub' | 'spoke' | 'personal';
}) {
  const state = fedLedgerState(users.length, localOnly.length);
  return (
    <section className="panel">
      <h3>
        {variant === 'personal'
          ? `Your fairness — served vs consumed (${winLabel})`
          : `Fairness — served vs consumed per user (${winLabel})`}
      </h3>
      <p className="dim">
        {variant === 'spoke' ? (
          <>
            Every request on capacity owned by someone else is double-entered: your users on
            hub-hosted models are <b>consumed</b> (the <b>Federation</b> account carries the
            remote serving side), and incoming hub traffic on backends you own is <b>served</b>{' '}
            for you. The Federation row mirrors this box’s account on the hub.
          </>
        ) : variant === 'personal' ? (
          <>
            Your side of the fairness ledger: <b>served</b> is your hosted models and owned
            backends serving other accounts (local and federated traffic alike),{' '}
            <b>consumed</b> is what you spent on hardware owned by others. Your own usage and
            everything attributable to nobody else is on the Usage page.
          </>
        ) : (
          <>
            Every request on a model hosted by another account counts as served for its host and
            consumed for the spender — local traffic and connected-server traffic alike. Using your
            own models, or backends with no fairness owner, is not listed here.
          </>
        )}
      </p>
      {state === 'no-usage' ? (
        <Empty label="No usage at all in this window — neither local traffic nor federation traffic." />
      ) : state === 'no-fed-activity' ? (
        <>
          <Empty label="Nothing to attribute between accounts in this window." />
          <p className="dim">
            {variant === 'personal' ? (
              <>You only used models on backends you own or without a fairness
                owner — that usage is on the Usage page.</>
            ) : (
              <>{localOnly.map(localOnlyLabel).join(', ')} only used models on backends
                they own or without a fairness owner — that usage is on the Usage page.</>
            )}
          </p>
        </>
      ) : (
        <>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th rowSpan={2}>User</th>
                  <th colSpan={4} className="num">Served</th>
                  <th colSpan={4} className="num">Consumed</th>
                  <th colSpan={4} className="num">Net (served − consumed)</th>
                </tr>
                <tr>
                  <th className="num">Req</th>
                  <th className="num">In new</th>
                  <th className="num">Out</th>
                  <th className="num">Cost</th>
                  <th className="num">Req</th>
                  <th className="num">In new</th>
                  <th className="num">Out</th>
                  <th className="num">Cost</th>
                  <th className="num">Req</th>
                  <th className="num">In new</th>
                  <th className="num">Out</th>
                  <th className="num">Cost</th>
                </tr>
              </thead>
              <tbody>
                {users.map((u) => (
                  <tr key={u.user_id}>
                    <td>{u.user}</td>
                    <td className="num">{fmtInt(u.served.requests)}</td>
                    <td className="num">{fmtInt(inNewTokens(u.served))}</td>
                    <td className="num">{fmtInt(u.served.completion_tokens)}</td>
                    <td className="num money">{fmtMoney(u.served.cost_micro, currency)}</td>
                    <td className="num">{fmtInt(u.consumed.requests)}</td>
                    <td className="num">{fmtInt(inNewTokens(u.consumed))}</td>
                    <td className="num">{fmtInt(u.consumed.completion_tokens)}</td>
                    <td className="num money">{fmtMoney(u.consumed.cost_micro, currency)}</td>
                    <NetCells served={u.served} consumed={u.consumed} currency={currency} />
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {variant === 'spoke' && (
            <p className="dim footnote">
              <b>Federation</b> is a virtual account — the remote side as seen from this box. Its
              figures equal this box’s owner account on the hub, seen from the other side.
            </p>
          )}
          {localOnly.length > 0 && (
            <p className="dim footnote">
              {variant === 'personal' ? (
                <>Not in this table — nothing attributable to you this window (own or
                  unowned-backend usage only). Your usage is on the Usage page.</>
              ) : (
                <>Not in this table — nothing attributable to them this window (own or
                  unowned-backend usage only): {localOnly.map(localOnlyLabel).join(', ')}.
                  Their usage is on the Usage page.</>
              )}
            </p>
          )}
        </>
      )}
    </section>
  );
}
