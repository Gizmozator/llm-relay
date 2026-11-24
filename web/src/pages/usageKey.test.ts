// Node's built-in test runner over the pure Key-cell decision (Node ≥ 23.6
// strips the types natively): run with `node --test "web/src/**/*.test.ts"`.
// Kept out of the vite bundle and the tsc pass (no @types/node here); the
// logic it pins is exactly what Usage.tsx renders.
import test from 'node:test';
import assert from 'node:assert/strict';
import { keyOrigin } from './usageKey.ts';

const row = (over: { key_name?: string; key_local?: boolean; username?: string }) => ({
  key_name: '',
  key_local: false,
  ...over,
});

test('real key shows its own name', () => {
  assert.equal(keyOrigin(row({ key_name: 'laptop', key_local: true, username: 'alice' })), 'name');
});

test('deleted key: local key id, key row gone', () => {
  assert.equal(keyOrigin(row({ key_name: '', key_local: true, username: 'alice' })), 'deleted');
});

test('spoke-origin row: stamped hub-side username, synthetic federation key', () => {
  // The ticket #15 case: a real username but no local key — must NOT read
  // as "deleted key" just because a username is present.
  assert.equal(keyOrigin(row({ key_name: '', key_local: false, username: 'alice' })), 'federation');
});

test('spoke-side row: no user, no key — still federation', () => {
  assert.equal(keyOrigin(row({ key_name: '', key_local: false })), 'federation');
});
