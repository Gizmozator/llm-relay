# Plan: Backend drain mode (graceful shutdown)

- **Status:** done
- **Owner:** antti
- **Started:** 2026-11-21
- **Issue:** —
- **Supersedes / superseded by:** extends 2026-11-21-agent-utilization-api-v1.md

## Goal

The admin can flip a per-backend **Draining** switch (Backends page) right
before shutting the engine's server down. While draining:

- **new sessions are refused** on that backend's models (503, OpenAI-style
  error, `Retry-After: 60`), and
- **sessions that are currently running keep working** — a request whose
  session id was active within the last **15 minutes** is allowed through,
  so in-flight conversations finish their remaining turns while nothing new
  starts.

Agents see it too: `/v1/utilization` and the MCP tool report a new
**`draining`** status (distinct from `offline`: the server is up, just not
accepting new conversations), and the live dashboard badges the backend's
models.

## Non-goals

- No scheduled/auto drain (manual switch only), no shutdown automation.
- No per-user or per-key exemptions — drain is a server-wide property of
  the backend.
- No queueing: refused requests are told to retry elsewhere/later.
- Drain state does not try to survive with perfect session memory across a
  relay restart (see Decisions #4 for the usage_log fallback).

## Current state

- `handle()` (internal/proxy/proxy.go): authKey → body → peek model →
  `store.ModelByName` (model + backend) → `Flight.Enter` → session id
  (`X-Session-Id` header, else `DeriveSession` = hash(key, model, first
  user message); `""` when not conversation-shaped) → forward.
- `store.Backend` has `Enabled`; `AdminUpdateBackend` takes pointer fields
  for partial updates; Backends page edits name/base_url/key/enabled.
- Migrations: embedded `internal/db/migrations/NNNN_name.sql`, next is
  `0006_backend_draining.sql`.
- Utilization core already loads `ListBackends` + health status per backend
  — the natural place to fold in a `draining` verdict.

## Approach

- **DB**: migration `0006_backend_draining.sql` →
  `ALTER TABLE backends ADD COLUMN draining INTEGER NOT NULL DEFAULT 0`.
  `store.Backend.Draining bool`; add to `backendCols`/scans;
  `UpdateBackend` gains `draining *bool`.
- **Session tracker** (`internal/proxy/sessions.go`):
  `SessionTracker{mu, seen map[string]time.Time}`; `Mark(session, now)`
  records activity (prunes entries older than the window, bounded map);
  `Recent(session, now)` = seen within **15 min** (`drainSessionWindow`).
  Handler always marks every proxied request's session (so the set is warm
  when drain flips on), and prunes lazily (every N marks).
- **Gate** in `handle()`, after the session id is computed and before
  forwarding: if `backend.Draining` and the session is not recent →
  `503` `{"error":{"message":"backend '<name>' is draining: no new
  sessions; existing sessions may continue"}}` + `Retry-After: 60`.
  Empty session id (`""`, non-conversation-shaped) counts as new → refused.
  Recent-session requests pass and refresh `Mark`.
- **Restart fallback**: on a memory miss during drain, allow if
  `usage_log` has a row for that session within the window
  (`SELECT 1 ... WHERE session_id=? AND ts>? LIMIT 1`) — covers relay
  restarts while drain is on; only runs on misses while draining.
- **Admin API**: `AdminUpdateBackend` accepts `draining` (bool).
- **Agent surface**: `ModelStatus` gains `draining` bool; status precedence
  `offline > draining > busy > free`. MCP tool description, agent guide
  (status table + decision logic: treat `draining` like busy-for-new-work,
  finish current sessions) and `docs/api-contract.md` updated.
- **Dashboard**: Backends page gets a Draining toggle (with badge);
  `/api/live` `models[]` gains `draining` and the live model cards show a
  "draining" badge.

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | migration 0006 + store Backend.Draining + UpdateBackend | — | ☑ |
| 2 | proxy SessionTracker + Mark on every request | — | ☑ |
| 3 | handle() drain gate (+ usage_log fallback) | 1,2 | ☑ |
| 4 | AdminUpdateBackend draining field | 1 | ☑ |
| 5 | utilization: draining field + status precedence | 1 | ☑ |
| 6 | live API models[].draining + Dashboard badge | 1 | ☑ |
| 7 | Backends page toggle + badge | 4 | ☑ |
| 8 | guide + api-contract + README + MCP description | 5 | ☑ |
| 9 | tests (store, gate allow/deny/window, fallback, utilization status, admin toggle) | 3,4,5 | ☑ |

## Verification

- `make ci` green; `cd web && npm run build` clean.
- New tests: store round-trip of `draining`; gate refuses unknown session
  with 503 + Retry-After, allows session marked < 15 min ago, refuses after
  window; usage_log fallback allows a session whose row is recent but not
  in memory; utilization reports `draining` (and `offline` still wins);
  admin toggle updates the flag.
- Manual smoke: prebuilt binary, enable drain via admin API, curl chat with
  fresh session → 503, same session twice → second allowed.

## Decisions

| # | decision | why | rejected alternative |
|---|---|---|---|
| 1 | Per-backend switch (not global) | owner phrasing: "admin backends new switch"; each backend = one engine server you shut down | global relay-wide drain (coarse; can't drain one engine of several) |
| 2 | "Old session" = active within last 15 min (owner) | matches "currently running" intent; bounded memory | unlimited session memory; "seen before drain flipped" (misses sparse conversations) |
| 3 | New `draining` status, not `offline` (owner) | agents should finish current sessions on it and avoid starting new ones — different advice than "skip server" | reusing `offline` (loses the distinction) or `busy` (wrong advice) |
| 4 | Drain flag in DB, session set in memory + usage_log fallback | switch must survive a relay restart honestly; memory tracker is the fast path, usage_log covers restarts | pure memory (silently off after restart); pure usage_log (long streams have no row until completion → mid-conversation lockout) |
| 5 | Refusal = 503 + Retry-After 60 | guide already tells agents to re-check on 429/503; semantically "temporarily unavailable" | 429 (rate-limit semantics are wrong here) |
| 6 | Empty derived session counts as new → refused during drain | "no new sessions" must not have a bypass via sessionless endpoints | allowing sessionless traffic (silent hole) |

## Open questions

- Should the dashboard's live view also refuse to start… no — dashboard
  users don't proxy inference; nothing to do there beyond the badge.

## Changelog

- 2026-11-21 — implemented as planned: migration `0006_backend_draining.sql`;
  `store.Backend.Draining` + `UpdateBackend(..., draining *bool)` +
  `store.SessionRecent`; `proxy.SessionTracker` (15-min window, lazy prune)
  marked on every proxied request; `handle()` gate → 503 + `Retry-After: 60`
  for new sessions on draining backends (usage_log fallback covers relay
  restarts); `ModelStatus.draining` + `draining` status
  (`offline > draining > busy > free`); live `models[].draining` + dashboard
  badge; Backends page toggle; guide/contract/README/MCP updated. Tests:
  `drain_test.go` (gate allow/deny/derived-session/fallback/undrain,
  utilization status), `sessions_internal_test.go` (window, prune, nil).
- 2026-11-21 — plan created.
