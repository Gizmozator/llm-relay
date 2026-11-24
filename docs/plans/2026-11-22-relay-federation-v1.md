# Plan: Relay federation — hub-and-spoke model sharing between relays

- **Status:** complete
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
| 1 | settings + peers store/migrations + token auth + admin/self-service API | #1 done | ☑ |
| 2 | WS channel + protocol + liveness + version negotiation (hub+spoke) | 1 | ☑ |
| 3 | catalog sync + materialization + RoundTripper tunnel + loop guard | 2 | ☑ |
| 4 | metering/attribution + served-vs-consumed aggregation | 3 | ☑ |
| 5 | UI: Federation control, Servers, My Keys, setup guide, spoke panel | 3,4 | ☑ |
| 6 | e2e (two local instances + mock engine) + docs + plan update | 5 | ☑ |

## Verification

- [x] `make ci` green at every commit
- [x] unit: token create/rotate/disable; type-scoped auth (server token rejected
      on `/v1`, API key rejected on `/federation`) — `internal/api/federation_test.go`:
      `TestPeerSelfService`, `TestAdminPeerManagement`, `TestFederationAuthMatrix`,
      `TestPeerAuthRevokedOnUserDisable`
- [x] unit: protocol version mismatch refused; frame codec round-trip incl.
      streaming chunks — `internal/fed/hub_test.go` `TestProtoMismatch`,
      `internal/fed/frames_test.go` `TestCodecRoundTrip`/`TestCodecBinaryIsBase64`
- [x] e2e (local, ports 18xxx, never touching production): hub+spoke up → spoke models
      appear in hub catalog; user on hub runs spoke model (stream + non-stream);
      same session → same member; spoke offline → models vanish; disable peer →
      channel drops; loop guard holds; served/consumed rows on both sides —
      `scripts/e2e-federation.sh` checks a–h (27 assertions, all PASS);
      frame-level loop guard + churn also unit-proven in `internal/fed/federation_test.go`
      (`TestLoopGuard`, `TestDisconnectVanishReconnectReturn`, `TestSpokeToSpokeTransit`)
- [x] UI smoke: role switch standalone↔hub↔spoke; guide hidden off-hub; My Keys
      creates both token types — role switch driven live through the API by the
      e2e script (PUT /api/admin/federation, live-safe), guide gate unit-tested
      (`TestFedRoleEndpoint`), both token types covered by `TestPeerSelfService`
      + `TestFederationAuthMatrix`; browser-level click-through not separately
      recorded

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
- 2026-11-23 — task 4 ocr triage: `FedUserTotals`/`FedConsumedSlice` now key by `user_id` (deleted users shared the `(deleted user)` label and collapsed, overwriting buckets) and emit `user_id` in the payloads; `fed_peer_id` adopts a UNIFORM convention — always the HOSTING peer, NULL when hub hardware served: spoke→hub-local consumed rows stamp NULL (was origin), mirror and transit served rows stamp the host (was origin), and `FedPeerTotals` redefines consumed as owner-level (every consumed row of the peer's owner; multi-peer owners carry the same total under each peer).
- 2026-11-23 — task 4 correction (C6 reversal): the task-3 guard rejecting synthetic members in admin groups (`assertNoSyntheticMembers`/`ErrSyntheticMember` → 400) contradicted the materialization design — a federated group IS a group whose members live on other relays — and is removed; admin groups accept `peer://` members. The FK hazard was already owned by the materializer (`DeleteFedModelsNotIn`/`PurgeSynthetic` clear memberships before deleting rows); a peer disconnect only disables the backend, so the group serves on from its remaining members. The spoke→synthetic-member double-entry now runs through a real admin group in the tests.
- 2026-11-23 — task 4 follow-up ruling: full double-entry on the hub — every hub row whose resolved member lives on a synthetic `peer://` backend (not just transit) mirrors a `served` row crediting the backend's owner (same counts, synthetic key); the consumed row's `fed_peer_id` names the hosting peer (fix for spoke→synthetic-member serves), the mirror's names the origin peer (NULL for hub-direct consumers). Written in the normal metering path (`proxy.fedOf`/`record`); transit keeps its manual pair (different code path, cannot double-write).
- 2026-11-23 — task 4 shipped: `usage_log` gains `fed_direction`/`fed_peer_id`/`fed_user` (migration 0013); hub stamps `consumed` on direct→synthetic and spoke→hub-local rows and writes the transit double-entry pair (counts parsed from the pumped frames); spokes meter `served` rows with the hub-stamped `fed_user` (the `req` frame gained the field; the hub is the only stamper). `internal/store/fedstats.go` adds the four rollups; `GET /api/admin/federation/stats` + `GET /api/federation/stats` serve them (role-dependent payload). Spoke-local consumption of federated models stays unstamped — the synthetic-backend scheme identifies that slice.
- 2026-11-23 — task 5 shipped (UI): new pages `AdminFederation` (role switch + spoke dial-in config with masked-but-editable token, live spoke channel status polled with the settings, spoke panels: shared models / served-by-source / my consumed; hub panel links to Servers), `Servers` (peer registry: online badge stamped from the hub registry, last-seen, version, expandable shared-model list derived from `fed_visibility`+allowlists, enable toggle, rotate shown-once, delete; ledger = per-user fairness table + per-peer rollup where the consumed columns carry the owner-level footnote and are never summed), `SetupGuide` (route `/setup`, all logged-in users, rendered only while role=hub via the new `GET /api/federation/role`). My Keys gained the credential-type choice (API key | server token, the choice shown on hubs) and the own-servers list with rotate/delete. Admin Models gained the per-model Share select (`off|all|allowlist` + peer multi-select; spoke tooltip says allowlist is effectively off), the `federation` badge on synthetic rows (only fed fields + enabled editable), and `peer: <name>` member locations in the group editors; the Backends page marks synthetic backends and hides their probe (Sync models). Usage: spoke-served rows (empty username/key on the wire) are filtered from the personal view and labelled `federation` in the admin detail. Go delta kept minimal: `GET /api/federation/role`, `spoke:{connected,hub_name,hub_version}` on `GET /api/admin/federation` (hello_ack identity published with the spoke channel via `Manager.SpokeStatus`), and `online` on `GET /api/admin/peers` (`Manager.Online`); contract updated. Deviation: the spoke status carries the hub hello_ack identity instead of a `last_seen` timestamp (the spoke has no peer row; last_seen is a hub-side field).
- 2026-11-23 — task 5 ocr triage (21 findings): every auxiliary load surfaces its failure instead of rendering as “no data” (transient poll failures keep the last-good payload + banner; AdminModels fails closed — rows stay synthetic-locked until the backend list loads); the spoke shared-list filters match the server (model+backend enabled, synthetic rows excluded on the hub side); the spoke consumed panel renders the admin-stats `consumed` rollup (all local users) instead of the caller-scoped `/api/federation/stats` round-trip; Share/allowlist controls are hidden on synthetic rows (the server ignores `fed_visibility` there); synthetic backends expose no Edit/Delete (base_url is the materializer key, delete 409s anyway); the dead personal-usage client filter is gone (the endpoint scopes by user; a client filter would desync the pager) and the admin `federation` label keys on the missing user, not the empty key name (deleted keys also yield ''); one canonical `isSyntheticBackendUrl` predicate mirrors `syntheticWhere`; stats-window select, spoke counts tables and form fields deduplicated; the sidebar role check refreshes on navigation.
- 2026-11-23 — task 6 shipped (close-out): `scripts/e2e-federation.sh` + `scripts/mock-engine.py` — a self-contained operator e2e that builds the real binary, boots two real relays and two mock OpenAI engines on 127.0.0.1:18xxx and asserts the full lifecycle (a: materialization in `/v1/models`; b: non-stream + SSE through the tunnel with usage counted; c: HRW stability — one session pins one member across 5 calls of a two-synthetic-member group; d/e: spoke kill → models vanish, restart → return, admin peer disable → channel drops, re-enable → reconnects; f: loop guard — spoke-origin traffic for spoke-hosted content never bounces (local serve / fast 404, no hang); g: served/consumed ledger on both sides incl. the hub mirror rows; h: plain local traffic stays out of the ledger) — 27/27 PASS, transcript `~/.cache/llm-relay/fed-e2e.log`; deliberately NOT wired into `make ci` (manual tool, boots real instances). New API tests for the thin status surfaces: `TestFedRoleEndpoint` (session-auth, role-only payload, no spoke-config leak, disconnected spoke branch) and `TestSpokeStatusAndPeerOnline` (real dialer→hub channel between two wired instances: connected + hello_ack identity on the spoke branch, `online` stamp on the admin peer list, both flip on disable). Docs: README Federation section + scripts/ layout + e2e task row; contract records the materialized-row enabled semantics (the next catalog push re-enables a per-model disable — the durable kill switch is the peer/backend disable). Task rows 1–3 retro-checked (shipped in 70ad909 / 3f773cd / 3afb99a — hygiene miss). Plan complete; the close-out lands on `feature/federation` and merges to `main` with the final gates.
- 2026-11-24 — post-merge final whole-repo `ocr scan` (15 chunks + budget re-run, 104 files / 31.3k lines): 274 findings triaged (0 critical / 12 high / 115 medium / 147 low; lows report-only). Adopted fixes committed reviewer-gated as `b0f41ae` (backend — federation slice: hub peer events dispatched off `h.mu` via an ordered worker; `Reload()` returns error and keeps the live dialer on transient settings-read failure; role not committed when the synthetic purge fails; transit pump watches the target Conn's Done (goroutine/in-flight-slot leak); transit meter window slides so >512 KiB responses carry their usage tail; peer headers filtered inbound as well as outbound; tunnel sends Cancel when the consumer aborts; `FedModelCollision` three-valued so a transient DB error keeps the live materialized row), `9df5fe7` (web hardening) and `05d4627` (e2e script: status assertions on every mutating call, bounded cleanup, wait_for metering polls, JWT secret raised to the new ≥32-byte floor; mock-engine rejects negative Content-Length). Full e2e re-run after the fixes: 44/44 PASS. Gitea #1 and #2 closed with the shipped summaries.
- 2026-11-24 — follow-up (Gitea #6): the sidebar role is now shared state instead of a per-page load. `web/src/fedrole.tsx` adds `FedRoleProvider`/`useFedRole` (mounted in `App` under `AuthProvider`): it loads `GET /api/federation/role` once a session exists and re-reads it on navigation, and `AdminFederation` publishes the role the `PUT` echoes back, so the hub-only **Connect server** entry appears and disappears the moment the role is saved — no reload, no stale link to the setup guide. The provider replaces the `useLoad` the sidebar carried; `Keys`/`AdminModels`/`SetupGuide` keep their own mount-scoped role loads (fresh on visit, untouched by this fix).
- 2026-11-24 — follow-up (Gitea #7): the admin **Federation** and **Servers** pages merged into one `AdminFederation` page (the two routes were tightly coupled — both loaded `fedSettings` + `fedAdminStats` and rendered role-aware served/consumed tables, and the Federation hub panel was just a paragraph linking to Servers). The page is now three role-aware sections: **Role & connection** (role switch + spoke dial-in config, live channel status, shared models), **Servers** (the peer registry — tokens, online state, shared models, enable/rotate/delete; hub only), and **Ledger** (served/consumed rollup — per-user fairness + per-server on a hub, served-to-hub + consumed-locally on a spoke). Panels moved verbatim from the two pages (behaviour-preserving), not rewritten; the double `fedSettings`/`settings`/`fedAdminStats` loads collapse to one each, and the spoke's served table is deduped (the Servers "This box served" and the Federation "Served to hub users" rendered the same `served_by_source` rows). The current role is read from the shared `useFedRole()` context (the #6 seam) instead of a page-local fetch, so a saved role re-shapes the sections at once. Role visibility is unchanged: hub sees all three sections, spoke sees role + connection + its ledger panels (no peer registry), standalone sees the role switch only. The duplicate sidebar **Servers** entry and the hub cross-link panel are dropped; `/admin/servers` stays as a `<Navigate replace>` redirect so old bookmarks keep working without polluting the back-stack. `web/src/pages/Servers.tsx` deleted; `SetupGuide`'s "hub's Servers page" wording follows the merge.
