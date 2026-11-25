# Plan: Spoke fairness — the ledger from the spoke's side, Federation virtual account

- **Status:** complete
- **Owner:** maintainer
- **Started:** 2026-11-25
- **Issue:** — (operator request while reviewing the fairness ledger on a spoke)
- **Supersedes / superseded by:** extends `2026-11-25-fairness-ledger-v1.md`
  (the ledger was hub-only in presentation; the rollup was already generic)

## Goal

A spoke admin must see the same fairness picture from their point of view:
how much their users spent on shared (hub-hosted) models, how much traffic
came in from the hub, and whether each account is net positive or negative.
Every consumed has a served — summed over the table, consumed == served —
and the spoke's **Federation** virtual account shows exactly what this
box's owner account shows on the hub (seen from the other side).
Every user additionally sees their **own** ledger row on a personal
**My fairness** page (operator follow-up: "federation statistics should be
visible to all users") — scoped data only, never other users' rows.

## Non-goals

- No hub-side fairness data pushed to the spoke (privacy by design — the
  spoke only ever sees its own traffic).
- No per-server table on a spoke (it has exactly one server: its hub).
- No real `users` row for Federation: display identity only, no login, no
  key, negative sentinel id (-1).

## Current state

The spoke stats view carried two box-level panels only (`served_by_source`,
`consumed`); the per-user fairness table was rendered for hubs only, even
though `FedUserTotals` is generic SQL. Spoke served rows (`user_id 0`)
folded into a phantom "(deleted user)" ledger entry; local users' spending
on hub-hosted models appeared nowhere in fairness.

## Approach

`FedUserTotals` gains two passes and one fix, so the spoke branch of both
stats endpoints can ship the same `FedLedger` the hub ships:

- **Fix:** the fed pass now skips rows with no local user (`user_id` 0 or
  NULL) — those are the spoke's incoming served rows, handled by the new
  incoming pass, never a user's.
- **Federation consumption pass:** an unstamped row on a synthetic backend
  (`peer://…`/`hub://…`) charges its consumer `consumed` and credits the
  Federation virtual account `served` (the remote side has no local
  identity). On a hub this is a no-op: the hub stamps those rows, and the
  counterpart there is a real peer-owner row.
- **Incoming pass:** a `served` row with no local user charges Federation
  `consumed`; the served side is already folded by the #12 hub-owner pass
  (a spoke served row has `fed_peer_id` NULL, so an owned serving backend
  credits its owner — "my engine served the hub"). Unowned serving
  backends leave that side uncredited: **the ledger balances exactly when
  every serving backend has a fairness owner.**
- Both passes skip the virtual fold when scoped (`userID != 0`): Federation
  is box-level and never appears in a personal view.
- API: the spoke branches of `GET /api/admin/federation/stats` and
  `GET /api/federation/stats` now carry `users`/`local_only` (admin) and
  `totals`/`local_only` (personal) beside the existing panels; the personal
  payloads also carry `currency` so the personal page needs no admin-only
  settings call.
- Web: the hub fairness table is extracted into the shared
  `components/FairnessPanel` (hub / spoke / personal variants) and rendered
  for spokes; the two box-level panels stay. New **My fairness** page
  (`/fairness`, nav entry on hub and spoke roles) renders the caller's own
  scoped row, plus their raw federated-model slice on a spoke.

## Tasks

- [x] `store`: fed-pass user_id fix, federation consumption pass, incoming
  pass, `FedVirtualUser`/`FedVirtualUserID` constants, doc rewrite.
- [x] `api`: spoke branches ship `FedLedger`; handler docs; personal
  payloads carry `currency`.
- [x] `web`: shared `components/FairnessPanel` (hub / spoke / personal
  variants), spoke payload types, spoke copy + Federation footnote; new
  **My fairness** page (`/fairness` route, nav entry on hub/spoke roles).
- [x] Review round 1: pass 3 excludes synthetic backends even when owned
  (a synthetic backend's owner never served — pass 4's Federation fold is
  the only correct credit; `TestFedUserTotalsOwnedSyntheticBackend` pins
  the single fold); pass 4 and `FedConsumedSlice` reuse the canonical
  `syntheticWhereB` predicate instead of a hand-rolled `hub://%` superset.
- [x] Tests: store `TestFedUserTotalsSpokeFairness` (both passes, balance
  gap = unowned incoming only, scoped views never show Federation); api
  spoke payload (users + Federation row, local_only drop, personal totals
  without the virtual row, currency present).
- [x] Docs: api-contract spoke payloads, README, this plan.

## Verification

- `make ci` green; hub tests (`TestHubGroupServeDoubleEntry`) unchanged —
  the new passes are provably no-ops on hub data (hub served rows name real
  users; hub synthetic-backend rows are stamped).
- Production check after deploy: the spoke's Federation row equals the
  spoke-owner account on the hub, mirrored.

## Decisions

- **Federation is a virtual display identity** (`user_id -1`), never a
  `users` row: no auth surface, no migration, no id collision.
- **Federated spending counts as consumed for the real user** (operator
  choice): it is their spending; the virtual account carries the remote
  served side so the table balances instead of hanging.
- **Balance gap is a feature flag for owners**: unowned serving backends
  are the only source of served≠consumed — the UI copy tells operators to
  set fairness owners.

## Open questions

- Whether the Usage page should also label `user_id 0` rows "Federation"
  (they currently read "(deleted user)") — display-only, decide with the
  next Usage pass.

## Changelog

- 2026-11-25: implemented (two store passes + virtual account, spoke API
  branches, shared FairnessPanel, tests, docs).
- 2026-11-25: operator follow-up — fairness visible to all users: personal
  **My fairness** page + `currency` on the personal payloads. Review round 1
  (REQUEST): synthetic-owner double-count fixed (pass 3 excludes synthetic
  backends) + regression test; canonical `syntheticWhereB` reused.
