// Node's built-in test runner over the pure fairness-panel decision (Node
// ≥ 23.6 strips the types natively): run with `node --test "web/src/**/*.test.ts"`.
// Kept out of the vite bundle and the tsc pass (no @types/node here); the
// logic it pins is exactly what AdminFederation.tsx renders (ticket #13).
import test from 'node:test';
import assert from 'node:assert/strict';
import { fedLedgerState, localOnlyLabel } from './fedLedger.ts';

test('ledger rows win: the table renders whatever else is around', () => {
  assert.equal(fedLedgerState(3, 0), 'rows');
  // A user with both fed and local usage is in the table; local-only
  // accounts become a footnote, not a different state.
  assert.equal(fedLedgerState(1, 2), 'rows');
});

test('no federation rows but local usage: explain, do not blank (the #13 case)', () => {
  assert.equal(fedLedgerState(0, 1), 'no-fed-activity');
  assert.equal(fedLedgerState(0, 5), 'no-fed-activity');
});

test('no federation rows and no local usage: there is simply no data', () => {
  assert.equal(fedLedgerState(0, 0), 'no-usage');
});

test('local-only label carries the account and its request count', () => {
  assert.equal(localOnlyLabel({ user: 'alice', counts: { requests: 12 } }), 'alice (12 req)');
});
