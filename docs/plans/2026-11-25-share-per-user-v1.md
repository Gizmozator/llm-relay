# Plan: Per-user share allowlist + "owner only" mode

- **Status:** complete
- **Owner:** maintainer
- **Started:** 2026-11-25
- **Issue:** Gitea #21
- **Supersedes / superseded by:** changes the sharing axis of
  `2026-11-22-relay-federation` (per-model Share switch + peer allowlist)

## Goal

Per-model **Share** currently offers local / all / allowlist-of-peers. Two
changes:

1. **Per-user allowlist** — the allowlist names *users*, not spokes. A
   spoke may use a model iff its **owner user** is on the list: "share
   this model with these accounts' relays". Survives peer renames and
   re-adds; one user with several boxes shares once.
2. **New "owner only" mode** (`fed_visibility='owner'`) — the model is
   shared only with its backend's fairness owner (ticket #12): that
   account's relays may pull it, nobody else.

## Non-goals

- No per-user ACL for *local* hub traffic — Share stays a federation gate;
  local visibility remains `visibility` (public/internal). "owner only"
  overlaps Visibility=internal on a single relay; its value is the
  federation case (the owner's spoke can pull the model, other peers
  cannot).
- No change to spoke→hub push: a spoke has no peer table, so `allowlist`
  and `owner` stay effectively local on a spoke (only `all` is pushed).

## Design

The hub only ever sees a spoke's **owner** (spoke-local usernames never
leave the spoke — the attribution authority is the spoke owner), so a
user allowlist maps to spokes exactly once: `peer.owner_user_id ∈ list`.

- `fed_visibility` CHECK widens to `('off','all','allowlist','owner')`.
- `fed_allowlist(model_id, peer_id)` becomes `fed_allowlist(model_id,
  user_id)` (FK → users, cascade on user delete). Migration 0018 maps
  existing rows through `peers.owner_user_id` (peers always have an
  owner — NOT NULL since 0011), deduping one-owner-several-peers.
- Sharing rules (hub side, `SharedWithPeer`):
  - `all` → every peer;
  - `allowlist` → peers whose owner user has a row;
  - `owner` → peers whose owner == the model's backend owner (backend
    without an owner → nobody);
  - spoke-side LocalOnly gate (peer id 0) keeps its semantics: no peers
    row with id 0 → both list modes false.
- Catalog push (`ListHubShareable`), group member visibility
  (`PeerSharedModelIDs`, `memberVisible`) and the serve-time check
  (`SharedWithPeer` via `fedShareVisible`) all follow the same rules.
- API: `PATCH /api/admin/models/{id}` accepts `fed_visibility='owner'`
  and `fed_user_ids` (replaces `fed_peer_ids`; still requires
  `fed_visibility='allowlist'` in the same call, unknown user id → 400).
  The model object carries `fed_user_ids`.
- Web: Share select gains "owner only"; the allowlist picker lists users
  (display name) instead of peers.

Migration mechanics: the runner executes each file in one transaction
with FK enforcement on, where `defer_foreign_keys` is a documented no-op
— so 0018 saves the two leaf children (`fed_allowlist`,
`model_group_members`) to TEMP tables, drops them, rebuilds `models` with
the widened CHECK (0010's sequence high-water dance included), recreates
the children and reinserts. Verified against a production-shaped seed.

## Tasks

- [x] Migration 0018 (user allowlist + widened CHECK), tested under
  runner conditions (FK on, one tx, data preserved, dedup, CHECK).
- [x] `store`: FedVisOwner, SetModelFedVisibility(user ids), Model struct
  `fed_user_ids`, SharedWithPeer rules, ListHubShareable,
  PeerSharedModelIDs, ListHubGroups backend owner, sharedModelsExpr.
- [x] `fed/catalog.go`: memberVisible owner case (peer owner vs backend
  owner).
- [x] `api`: fed_user_ids validation + docs; error strings.
- [x] `web`: api.ts types, Share select "owner only", user picker.
- [x] Tests: store matrix (allowlist/owner × peer owner match/mismatch,
  unowned backend, peer 0), api PATCH, forged-origin regression on user
  semantics.
- [x] Docs: api-contract, README federation paragraph, this plan.

## Verification

- `make ci` green; migration test applies 0001→0018 on a seeded DB.
- Prod check: existing allowlist shares survive as owner-user rows;
  a spoke whose owner is listed still sees the model after redeploy.

## Decisions

- Allowlist names users because the spoke owner is the only identity the
  hub can attribute to (ticket design note); peer ids were an
  implementation detail leaking into the admin model.
- `owner` mode requires a backend owner; without one it shares with
  nobody (same fail-closed direction as `off`).
- Migration drops nothing silently that still has meaning: every peer has
  an owner (NOT NULL), so every existing allowlist row maps.

## Open questions

- (none)

## Changelog

- 2026-11-25: plan written; migration 0018 authored and verified.
- 2026-11-25: store (sharing rules, PeerSharedModelIDs, sharedModelsExpr),
  api (fed_user_ids wire), web (owner-only option + user picker), tests
  and docs shipped; reviewer APPROVE. sharedModelsExpr's allowlist term
  re-checks fed_visibility='allowlist' so the count can never exceed what
  SharedWithPeer actually serves.
