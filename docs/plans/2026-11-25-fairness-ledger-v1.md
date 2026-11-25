# Plan: Fairness ledger — all cross-user traffic, calendar-month windows

- **Status:** complete
- **Owner:** maintainer
- **Started:** 2026-11-25
- **Issue:** — (operator request after the first federation ledger review)
- **Supersedes / superseded by:** amends the fairness scope of
  `2026-11-22-relay-federation-v1.md` (Decision: local traffic stays out of
  the ledger — now reversed for cross-user local traffic)

## Goal

The federation ledger must answer "who is giving capacity to whom" for the
whole hub, not only for cross-peer flows. If user A hosts a model and user B
uses it — directly on the hub **or** through B's connected spoke — that is
A serving and B consuming, and both must see it in the fairness table.
Second ask: the fairness page must be reviewable per calendar month (this
month vs last month), not only as a rolling N-day window.

## Non-goals

- No billing/settlement — still fairness bookkeeping.
- No spoke-side fairness rework: the spoke views (served-to-hub-sources,
  consumed-on-federated-models) keep their hub-facing meaning.
- No graphs (a monthly served/consumed/net chart is a candidate follow-up).
- No write-time stamping change: attribution stays a query-time join, so
  historical rows are credited the moment a backend owner is set.

## Current state

The ledger (`store.FedUserTotals`) summed only `fed_direction != ''` rows
plus the ticket #12 hub-owner credit (consumed rows with `fed_peer_id` NULL
credited to the serving backend's owner). Hub-local rows (`fed_direction`
empty) never entered it: a hub user spending 150M tokens on another user's
owned backend showed nowhere in fairness — `FedLocalOnlyUsers` merely named
such accounts as "usage present, ledger absent". The stats window was
`?days=N` only (open upper bound).

## Approach

- **Local cross-user pass** (third pass in `FedUserTotals`): a plain row
  whose backend has an owner other than the consuming user is double-entered
  — `owner.served += row`, `consumer.consumed += row`. One range scan +
  join + GROUP BY (same shape as the hub-owner pass; no per-row subquery).
  Excluded: self-use (owner == consumer), ownerless backends, anonymous rows
  (`user_id` NULL/0). Disjoint from the fed passes by the `fed_direction`
  filter, so no row is ever counted twice.
- **`FedLedger`** returns the ledger + local-only list as one consistent
  pair: anyone the ledger shows (either side, either pass) is dropped from
  `local_only`. This also fixes the pre-existing #12/#13 overlap where a
  hub-owner with self-use rows could appear in both.
- **Calendar windows**: `?month=YYYY-MM` (UTC month, `[first, first of next)`)
  on both stats endpoints, `month` winning over `days`; every rollup query
  gained an exclusive `until` bound (`""` = open). Responses echo `until`.
- **UI**: the stats-window select gains "this month" / "last month" presets
  (`StatsWindow` type in `components/ui`); panel headings show the window
  label; the fairness panel explains the rule and the local-only copy says
  "nothing attributable" instead of "local usage is not part of this ledger".

## Tasks

- [x] `store.FedUserTotals`: local cross-user pass; `until` on all five
  rollups; `FedLedger` consistency helper.
- [x] `api`: `fedWindow` (month param), both stats handlers on `FedLedger`,
  payload carries `until`.
- [x] Tests: store (local pass both scopes + bounded window, `FedLedger`
  filter), api (month validation, endpoint-level credit + month window),
  fed e2e signature updates.
- [x] Web: `StatsWindow` select + labels, api query pair, ledger copy.
- [x] Docs: this plan; `docs/api-contract.md` federation-stats section
  (month param, `until` echo, local cross-user pass, narrowed `local_only`);
  README fairness-ledger paragraph; federation plan amended by reference.

## Verification

- `make ci` green (fmt/vet/go test incl. new store + api tests/race, node tests).
- New tests prove: double-entry on local cross-user rows (all six count
  columns), self-use/ownerless/anonymous/out-of-window exclusion, scoped
  queries on both sides, `until` bound, `local_only` never naming a ledger
  member, month validation (junk → 400, valid → 200) and a closed month
  window carrying the row.
- Production check after deploy: the hub fairness table shows the backend
  owner's served growing by local cross-user traffic (the 150M-token case).

## Decisions

- **Count all cross-user traffic** (local-direct + spoke), not just
  cross-peer: the operator asked for hosted-model usage to be visible
  regardless of transport. Supersedes the federation plan's "plain local
  traffic is not ledger traffic".
- **Self-use excluded** (owner == consumer): serving yourself is neither
  contribution nor consumption; it cancels on Net anyway. (Owner choice:
  "whatever makes sense".)
- **Month presets are additive**: the rolling-day windows stay; `month`
  wins when both params are present.
- **UTC calendar months**: ts is stored UTC; no per-user timezone.
- **`local_only` semantics narrowed** to "usage attributable to nobody
  else" (self-use or ownerless backends); the filter lives in `FedLedger`,
  not the SQL, because host-side ledger membership is invisible to a
  consumer-grouped query.

## Open questions

- Monthly served/consumed/net **chart** on the fairness page (LineChart +
  chart.js already exist) — deferred, decide after one month of real data.
- Whether ownerless backends should get a default owner (e.g. the admin) so
  hub-wide hardware also credits someone — operator policy, not code.

## Changelog

- 2026-11-25: implemented in one pass (store pass + FedLedger, fedWindow
  month param + until bounds, UI month presets + copy). Reviewer round 1
  REQUEST: `docs/api-contract.md` still described the old contract — fixed
  (month param + `until` echo, the three ledger passes, narrowed
  `local_only`), README ledger paragraph updated in the same pass.
  Deployed with the next `:latest` image.
