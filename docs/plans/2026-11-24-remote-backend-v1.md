# Plan: Remote backends over WebSocket (headless v2)

- **Status:** active
- **Owner:** maintainer
- **Started:** 2026-11-24
- **Issue:** —
- **Supersedes / superseded by:** supersedes `2026-11-24-headless-spoke-v1.md` (its env-bootstrap
  headless mode is removed by this plan)

## Goal

A machine behind NAT (or any box that just runs an engine) can expose its local
OpenAI-compatible engine to a relay **without opening any inbound port**. An
admin creates a **remote backend** on the Backends page and gets a one-time
connect token; the remote box runs the same `llm-relay` binary in headless
mode with that token and dials the relay over a WebSocket. The backend then
appears on the Backends page like any other: it shows online while connected,
**Probe / Sync models** discovers its models through the tunnel, and the relay
routes chat/completions traffic to it over the same WebSocket. Any relay can
accept remote backends regardless of federation role — standalone, hub or
spoke.

## Non-goals

- Multiple engines behind one headless connection (one connector = one engine
  URL; run one connector per engine).
- Model catalog or pricing on the headless box — the server owns the catalog;
  the connector is a dumb pipe to its engine.
- Reusing federation peer tokens (`fed-`) for remote backends — new token
  kind, new surface, see Decisions.
- Anything the old headless spoke did (env-written backend/model rows, spoke
  role bootstrap) — removed, not deprecated.

## Current state

- `internal/headless` (plan 2026-11-24-headless-spoke): env → DB bootstrap that
  creates one backend + one model + spoke federation settings; the box then
  joins as a spoke via the ordinary dialer. Wired in `cmd/llm-relay/main.go`
  (bootstrap + admin listener suppression). Users found this shape low-value.
- `internal/fed`: the federation WebSocket stack — `Conn` (writer goroutine,
  read pump, pinger, request-id multiplexing, `MaxInflight=16`), JSON frames
  (`hello/hello_ack/req/res/res_body/res_end/cancel/ping/pong`, `MaxBody`
  ≈3 MiB), the hub registry and the spoke dialer. `fed.servedWriter` shows the
  res-frame pattern; `fed.tunnel.RoundTripFed` shows the client side.
- `internal/proxy`: shared `Client` with a `schemeDispatch` transport —
  `peer://`/`hub://` go to the fed tunnel, everything else plain HTTP.
  `HealthCache` answers synthetic backends from the channel registry;
  `eligibleMembers` gates group members on the same cache. `proxy.Probe`
  (health + /v1/models) is the admin discovery probe.
- `internal/store`: `backends(id, name UNIQUE, base_url, api_key, enabled,
  draining, last_probe_*, server_version, created_at)`; synthetic backends are
  keyed by their `base_url` scheme (`peer://<id>`, `hub://federation`).
- Admin API: backend CRUD + `POST /backends/{id}/probe` + `POST /backends/{id}/models`
  (bulk add discovered ids). Dashboard `Backends.tsx` renders the table,
  add/edit modal and the Sync (probe → pick → add) modal.

## Approach

**Wire protocol:** reuse the `fed` frame codec and `fed.Conn` verbatim on a new
endpoint. The remote connector is a *backend*, not a federation peer: no
catalog/status frames, no role gate, no peer rows.

**Server side (any role):**

- New relay-port route `GET /remote/v1/connect`, behind a new `RemoteAuth`
  middleware (bearer `rb-…` token → `store.BackendByRemoteTokenHash`).
- New package `internal/remote`:
  - `Registry` — live `*fed.Conn` per backend id (replace-on-reconnect, Kick,
    CloseAll, throttled `RecordProbe`-style touch). `ServeConn` runs the
    hello/hello_ack handshake and registers.
  - `RoundTrip(req)` — `remote://<id>` → Req frame → Res/ResBody/ResEnd stream
      (dial-class errors so the group failover treats a dead channel like a
      dead socket). `Cancel` on consumer hang-up.
  - `Probe(ctx, id)` — same shape as `proxy.Probe` (GET `/health` +
    `/v1/models` through the tunnel) by refactoring `proxy.Probe` into
    `ProbeWith(client, base)` + thin wrappers.
- `proxy`: `schemeDispatch` routes scheme `remote` to a new `Remote` seam
  (implemented by `*remote.Registry`); `joinBackendURL` treats `remote://`
  like synthetic (append verbatim); `HealthCache` gets a `Remote` hook —
  online = live channel (same rule as synthetic).
- Admin API: `POST /api/admin/backends/remote {name}` creates the row
  (`base_url = remote://<id>`, token shown once, hash stored) and returns the
  token; `POST /api/admin/backends/{id}/remote/rotate` replaces it; probe
  branches to the tunnel for `remote://` backends; delete/disable/rotate kick
  the live channel. `AdminUpdateBackend` refuses base_url edits on remote
  rows (the URL is the registry key).
- Migration `0015_remote_backends.sql`: `ALTER TABLE backends ADD COLUMN
  remote_token_hash TEXT` + `CREATE UNIQUE INDEX` (NULLs distinct → plain
  backends unaffected).

**Client side (headless mode, same binary):** `HEADLESS=true` now means
*connector*: config requires `RELAY_URL`, `RELAY_TOKEN`, `ENGINE_URL`
(optional `ENGINE_TOKEN`); no DB, no listeners, no federation manager. The
connector dials `<RELAY_URL>/remote/v1/connect`, runs hello, then serves Req
frames against `ENGINE_URL` (paths restricted to `/v1/*` and `/health`,
hop-by-hop headers stripped, `ENGINE_TOKEN` applied as the upstream key),
streams ResBody chunks back, honors Cancel, and redials with jittered
exponential backoff (same ladder as the fed dialer).

**Removed:** `internal/headless` (bootstrap + tests), the old env block
(`BACKEND_URL/BACKEND_TOKEN/MODEL_NAME/HUB_URL/HUB_TOKEN/PRICE_*`),
`deploy/headless-spoke/`, `docs/workflows/headless-spoke.md`, README headless
section, `.env.example` block. Replacements: `deploy/remote-backend/`
(compose + env for the connector box) and `docs/workflows/remote-backend.md`.

**Dashboard:** Backends page gains “+ Remote backend” (name → token shown
once, copy button), a `remote` badge on the row, Sync/Edit/Delete for remote
rows (Edit = rename + rotate token, no base_url), and the live Status badge
comes from the registry via the health cache.

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | migration 0015 + store: remote base_url helpers, create/rotate/lookup-by-token | — | ☑ |
| 2 | `internal/remote`: Registry (ServeConn, RoundTrip, Kick/CloseAll) + client connector | 1 | ☑ |
| 3 | proxy seams: schemeDispatch `remote`, joinBackendURL, HealthCache hook, Probe refactor | 2 | ☑ |
| 4 | api: RemoteAuth + connect route + admin create/rotate/probe/delete wiring | 2 | ☑ |
| 5 | main.go: headless connector mode (no DB/listeners), wire registry into relay mux + admin API + proxy | 3,4 | ☑ |
| 6 | strip old headless: package, config env block, deploy/, workflow doc, README, .env.example | 5 | ☑ |
| 7 | dashboard: remote create modal + token reveal, badge, edit/rotate, probe works unchanged | 4 | ☑ |
| 8 | tests: store, registry e2e (fake engine), proxy routing, api auth | 5,6,7 | ☑ |
| 9 | docs: README, workflows/README, new workflow doc, AGENTS tree, plan update | 8 | ☑ |

## Verification

- [x] `make ci` green
- [x] e2e (test): fake engine (httptest) + connector + registry in one process:
      connect → probe lists engine models → chat completion round-trips
      (stream + non-stream) → cancel mid-stream → kill channel → RoundTrip
      fails dial-class → reconnect restores service.
- [x] store: token shown once, hash lookup, rotate invalidates old token,
      second connector with same token replaces the first.
- [x] api: 401 without/with wrong token; connect refused for disabled backend
      (403); probe on offline remote backend answers a clean error, not a hang.
- [x] manual: `HEADLESS=true RELAY_URL=… RELAY_TOKEN=*** ENGINE_URL=…`
      against a dev relay → Backends page shows it online → Sync models →
      chat through the relay works.

## Decisions

| # | decision | why | alternatives considered |
|---|---|---|---|
| 1 | New token kind `rb-…` stored hashed on the backend row | the admin who owns the backend owns the token; self-describing prefix keeps surfaces apart (sk-/fed-/rb-); hash-at-rest matches api_keys/peers | reuse `fed-` peer tokens (peers are federation members with owner users and hub-only connect — wrong shape); plaintext token in `api_key` (plaintext-at-rest, wrong column semantics) |
| 2 | Reuse `fed.Conn` + frames on a new endpoint | writer/pinger/multiplex/backpressure already built and tested; one wire codec in the codebase | raw HTTP/2 reverse proxy (new protocol, new failure modes); reuse `/federation/v1/connect` (role-gated, peer-authed, catalog semantics — wrong contract) |
| 3 | `remote://<backend-id>` synthetic-style base_url, but NOT in `syntheticWhere` | remote models are ordinary local rows: shareable via federation, group-eligible, priced locally; only the transport is synthetic | fold into `SyntheticBaseURL` (would wrongly exclude remote models from catalog sharing and probe paths) |
| 4 | Headless mode binds nothing and opens no DB | the connector's only job is the outbound channel; a local /v1 surface with an empty catalog is dead weight | keep the relay listener "for compatibility" (nothing to serve) |
| 5 | Health = live channel (registry), like synthetic backends | the WS connection IS reachability for a NATted box; a tunnel probe every minute would burn the socket | periodic /health over the tunnel (extra traffic, same answer) |
| 6 | Connector restricts served paths to `/v1/*` + `/health` and forwards only to its configured engine URL | a hostile/compromised relay must not use the tunnel to scan the connector's LAN (SSRF) | forward any path (no real benefit, adds risk) |
| 7 | One connector = one engine URL | keeps the pipe dumb and the config honest; multiple GPUs → multiple connectors | multi-engine fan-out in the connector (re-implements the relay on the wrong side) |

## Open questions

- Token expiry / rotation reminders? Deferred: rotate is manual, same posture
  as peer tokens.

## Changelog

- 2026-11-24 — plan created (headless v2: remote backends over WebSocket).
- 2026-11-24 — implemented: migration 0015, `internal/remote` (registry +
  connector), proxy/api/main wiring, old headless removed, dashboard remote
  backend UI, tests (store/registry e2e/proxy/api) green under `make ci`.
  Manual two-box check still open.
- 2026-11-24 — review round 1 fixes: admin/live health cache now gets the
  `Remote` hook (Backends page reads the registry, offline for a
  never-connected backend); per-connection `Touch` sweep revalidates the
  backend row against the connect-time token hash (rotate/disable/delete
  that raced past the kick closes the channel "revoked"); dead
  `store.BackendByName` removed; old plan marked superseded bidirectionally.
- 2026-11-24 — ocr findings (2 passes, 9 findings total, all adopted):
  headless URL-shape validation + address-clash skip; 503 on create without
  registry; bounded tunneled probe; connector path gate rejects dot
  segments (raw and %-encoded); connector sheds req frames beyond
  `fed.MaxInflight` (hostile-relay backpressure); connect endpoint carries
  a per-IP token-bucket throttle (own bucket space, reset on successful
  auth); admin `base_url` refusal only on real change (whole-object PATCH
  echo works); `EngineToken` doc corrected (Bearer only). Gate hardened
  further per reviewer note: pre-encoded `%2e%2e` frame paths (hostile
  relay crafting frames directly) are refused via the unescaped form too.
