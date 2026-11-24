// Pure decision logic for the hub fairness ledger (ticket #13), extracted
// from AdminFederation.tsx so `node --test` can pin it (the same seam as
// usageKey.ts — no imports, so Node's type stripping runs it directly).
// The ledger only aggregates federation rows, so an account whose window
// usage is entirely hub-local is absent from it by design. The UI must say
// "no federation activity — this account's usage is local" rather than show
// a blank that reads as a bug, and must keep that state apart from "no data
// at all".

/** The three honest states of the hub fairness panel. */
export type FedLedgerState = 'rows' | 'no-fed-activity' | 'no-usage';

/**
 * Decide the fairness panel's state from the two lists the hub stats
 * payload carries: ledger rows (users with federation traffic) and
 * local-only accounts (hub-local usage, no federation rows). Ledger rows
 * win: a user with both is already in the table.
 */
export function fedLedgerState(ledgerRows: number, localOnly: number): FedLedgerState {
  if (ledgerRows > 0) return 'rows';
  if (localOnly > 0) return 'no-fed-activity';
  return 'no-usage';
}

/** "alice (12 req)" — one local-only account inside the explanation list. */
export function localOnlyLabel(r: { user: string; counts: { requests: number } }): string {
  return `${r.user} (${r.counts.requests} req)`;
}
