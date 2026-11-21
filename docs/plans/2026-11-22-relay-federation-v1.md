# Plan: Relay federation — hub-and-spoke model sharing between relays

- **Status:** active
- **Owner:** maintainer
- **Started:** 2026-11-22
- **Issue:** #2 (blocked by #1 / model-groups plan)
- **Supersedes / superseded by:** —

## Goal

Multiple llm-relay instances form a private federation: a public **hub** and LAN
**spokes**. A spoke dials out to the hub with a server token and shares selected
models; the merged pool is reachable from every member — a user on any relay can
use any shared model anywhere. Users see only models; admins see the cluster.
Usage tracking is fairness bookkeeping (served vs consumed per user), not billing.

## Non-goals

- No direct spoke↔spoke connections (transit via hub), no cross-relay end-user
  identity, no budgets/quotas/settlement, no open enrollment, no nested hubs.

## Current state

- Single-instance relay: env config (`internal/config`), DB-backed backends/
  models/groups (#1), proxy with drain/health/in-flight, admin API + React
  dashboard. No instance-role concept. A `settings(key, value)` table exists
  (0001, seeded `currency`) with store get/put helpers and
  `GET/PUT /api/admin/settings` — instance role + spoke config live there.
- The proxy has one shared `Handler.Client` with a single `Transport`
  (dial/TLS timeouts only). The RoundTripper seam is a **scheme-dispatching
  wrapper** installed on that transport: handle `peer://` in the wrapper,
  delegate everything else to the inner default transport.

## Approach

- **Instance role** (existing `settings` table): `standalone` (default) | `hub`
  | `spoke`,
  set on a new admin **Federation** page. Spoke mode stores hub URL + server
  token + share opt-in (model ids). Role change is live-safe: spoke→standalone
  drops the channel; hub mode enables `/federation/*` endpoints.
- **Enrollment**: `peers(id, name, owner_user_id NOT NULL, key_hash, enabled,
  last_seen, version, created_at)`. Server tokens are self-service on **My
  Keys** (type-scoped: API keys authenticate `/v1/*` only, server tokens the
  federation channel only; shown once, stored hashed). Admins list/disable/
  rotate any peer on the **Servers** page.
- **Channel**: spoke dials `GET /federation/v1/connect` (WebSocket upgrade) with
  the token; hello frame negotiates protocol version (refuse unknown) and
  reports instance info; hub↔spoke frames: `catalog` (shared model list),
  `status` (peer liveness for propagation), `req`/`res`/`res_body`/`res_end`
  (tunneled HTTP, request-id keyed, streaming chunks), `ping`/`pong`. One
  multiplexed channel per peer; per-peer in-flight cap; ping/pong liveness.
- **Materialization (the key integration)**: each online peer gets a synthetic
  backend row (`base_url = peer://<peer-id>`) and its advertised models are
  ordinary `models` rows on that backend (name, upstream name, pricing, context,
  bench synced from the catalog frame). Group members may reference these rows —
  so HRW affinity, drain, health, weights, usage `model_id` all work unchanged.
  Peer disconnect/disable → synthetic backend + its model rows are removed
  (hidden) and vanish from `/v1/models`. **Collision policy for materialized
  names**: a shared model whose advertised name collides with an existing group
  name or a local (non-peer) public model name is NOT materialized — it is
  skipped with a logged warning naming the model and the colliding name, and
  the serving admin resolves it by renaming on their side. Two peers
  advertising the same name is fine (that is the grouping use case: distinct
  `peer://` backends, same name, poolable into one group). The spoke side
  applies the same rule against its local catalog. The spoke side does the same for the
  hub's catalog (backend `hub://`), enabling local-first resolution: spoke
  resolves groups locally when a local member exists, else forwards to the hub.
- **Tunnel**: hub-side `http.RoundTripper` for `peer://` backends writes a `req`
  frame (member id, method, path, headers, body) and streams the response back.
  Spoke-side serves frames against its own local backends (applies stored
  backend API keys locally — keys never leave the spoke). Loop guard: forwarded
  requests carry the origin peer id; the hub never routes back to it.
- **Metering/attribution**: usage rows for federation traffic gain
  `fed_direction` (`served`|`consumed`), `fed_peer_id`, `fed_user` (stamped
  hub-side identity: direct user or peer owner — spoke-local usernames never
  leave the spoke). Hub aggregates **served vs consumed per user** (Servers
  page + user's own stats). Spoke dashboards show both directions locally.
  No enforcement.
- **UI**: hub — Federation control (role), Servers page (peers, online state,
  version, shared models, served/consumed stats; admin-only), setup guide
  ("set up your home server", all logged-in users, rendered only when role =
  hub, agent-guide pattern); spoke — Federation panel (connection status,
  shared models, served stats) + hub-catalog models appear in normal lists;
  spoke model rows carry a per-model **federation visibility switch** (the
  share opt-in list as its backing store — paid-subscription backends stay
  local-only and invisible to the federation while local engines are shared);
  My Keys — create API key **or** server token, own servers listed with
  rotate/revoke. Users see models only; cluster status is admin-only.
- **Security**: closed group (user accounts admin-controlled), share opt-in
  with per-model caps, tokens hashed + rotatable, federation endpoint hardened
  like the public relay (body caps, timeouts, auth), versioned protocol.

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | settings + peers store/migrations + token auth + admin/self-service API | #1 done | ☐ |
| 2 | WS channel + protocol + liveness + version negotiation (hub+spoke) | 1 | ☐ |
| 3 | catalog sync + materialization + RoundTripper tunnel + loop guard | 2 | ☐ |
| 4 | metering/attribution + served-vs-consumed aggregation | 3 | ☐ |
| 5 | UI: Federation control, Servers, My Keys, setup guide, spoke panel | 3,4 | ☐ |
| 6 | e2e (two local instances + mock engine) + docs + plan update | 5 | ☐ |

## Verification

- [ ] `make ci` green at every commit
- [ ] unit: token create/rotate/disable; type-scoped auth (server token rejected
      on `/v1`, API key rejected on `/federation`)
- [ ] unit: protocol version mismatch refused; frame codec round-trip incl.
      streaming chunks
- [ ] e2e (local, ports 18xxx, never touching production): hub+spoke up → spoke models
      appear in hub catalog; user on hub runs spoke model (stream + non-stream);
      same session → same member; spoke offline → models vanish; disable peer →
      channel drops; loop guard holds; served/consumed rows on both sides
- [ ] UI smoke: role switch standalone↔hub↔spoke; guide hidden off-hub; My Keys
      creates both token types

## Decisions

| # | decision | why | alternatives considered |
|---|---|---|---|
| 1 | remote models materialized as model rows on synthetic `peer://` backends | reuses #1 routing/affinity/metering wholesale; RoundTripper is the only new transport | special-case remote members in the router (parallel machinery) |
| 2 | peer maps to one hub user (mandatory); spoke traffic = owner user | people contribute hardware; fairness view per person | synthetic peer accounts; federated end-user identity (too heavy) |
| 3 | one account, two access paths; secrets stay type-scoped | entitlements path-independent; leaked client key must not enroll peers | single shared secret (pool-poisoning risk) |
| 4 | tracking only, no enforcement | relay has no budget machinery; social controls suffice | soft/hard caps (deferred) |
| 5 | users see models, admins see cluster | availability is the user signal; topology is noise | user-visible peer status (superseded by owner) |

## Open questions

- Per-peer sharing scopes: promoted to v1 as the approval mechanism — per-model
  federation visibility `off` (default) | `all` | `allowlist`, symmetric on hub
  and spokes; hub refuses peer requests for models not shared with that peer.

## Changelog

- 2026-11-22 — plan created from Gitea #2 design.
