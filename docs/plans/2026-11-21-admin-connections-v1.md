# Plan: Admin active-connections view

- **Status:** done
- **Owner:** antti
- **Started:** 2026-11-21
- **Issue:** —
- **Supersedes / superseded by:** extends 2026-11-21-live-dashboard-v2-usage-graph.md

## Goal

An admin opens the dashboard and sees the **active connections**: which
users are using LLM streams right now, on which models — plus the key that
made the call, endpoint, whether it is streaming, session id, client IP and
how long it has been running. Personal (non-admin) views keep showing only
the per-model counts they already have.

## Non-goals

- No history/audit (that is the Usage pages); this is "right now" only.
- No new persistence — same in-memory design as the existing active counts
  (a relay restart clears it, which is correct: the streams died with it).
- No per-user "your connections" view — users already see aggregate active
  counts; the detail (other users' names) is admin-only privacy.

## Current state

- `proxy.InFlight` stores `map[(userID,model)][]start` — enough for
  `Snapshot` (counts) and `BusySince` (drain/utilization), not for listing.
- `/api/admin/live` already polls every 5 s for admins and is the natural
  carrier; `handle()` knows user, key, model, endpoint, session, stream
  flag and client IP at request start.

## Approach

- **Richer tracker** (`inflight.go`): store full records
  `Stream{ID, UserID, User, Key, Model, Endpoint, Session, IP, Streaming,
  Start}` in `map[int64]*Stream`. `Enter(Stream) int64` /
  `EnterAt(Stream, start)` return an id; `Leave(id)` removes it.
  `Snapshot`/`BusySince` keep their signatures (derived from records);
  new `List(userID)` returns copies sorted by start (0 = all users).
- **handle()**: compute session id first (drain gate needs it), then
  `id := Flight.Enter(streamInfo)` + `defer Flight.Leave(id)`. Refused
  (drain) requests never enter the tracker.
- **API**: `FlightSource` gains `List`; `/api/admin/live` response gains
  `connections: [{user, key, model, endpoint, session, ip, streaming,
  started_at, seconds}]` (admin route only — same role+prefix guard as
  `user_id`; personal `/api/live` omits the key entirely) and
  `last_used: [{user, last_used}]` — per-user most recent usage row within
  the last 24 h (`store.LastUsedByUser`), so the panel also answers "who
  used this recently", not just right now.
- **Dashboard**: admin-only **Active connections** panel (table, sorted by
  longest-running first, empty state "No active requests") plus a compact
  **Last used** list; both refresh automatically with the existing 5 s
  live poll (no extra polling loop).

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | InFlight records + Enter/Leave(id)/List (keep Snapshot/BusySince) | — | ☑ |
| 2 | handle() wiring (session before Enter, full Stream info) | 1 | ☑ |
| 3 | /api/admin/live connections + last_used fields (admin-only) | 1 | ☑ |
| 4 | web: types + admin panel on Dashboard (auto-refresh via live poll) | 3 | ☑ |
| 5 | tests (tracker, e2e hook, admin sees / user doesn't) | 2,3 | ☑ |
| 6 | docs: api-contract + README + changelog | 3 | ☑ |

## Verification

- `make ci` green; `cd web && npm run build` clean.
- Tests: tracker Enter/List/Snapshot/BusySince/Leave by id; e2e hook still
  counts during upstream stall; admin live response contains the entered
  stream with user/model/seconds; personal live response has no
  `connections` key.

## Decisions

| # | decision | why | rejected alternative |
|---|---|---|---|
| 1 | Carried inside `/api/admin/live` | already admin-gated + polled; no new route/poll loop | separate `/api/admin/connections` endpoint |
| 2 | Admin-only detail | connection rows name other users; the live view is shared with all users | showing connections to everyone (privacy leak) |
| 3 | Leave by opaque id, not (user,model) | exact pairing under concurrency; oldest-release heuristic no longer needed | keep (user,model) removal heuristic |
| 4 | In-memory only | matches active-count design; restart kills the streams anyway | persisting connections (meaningless across restarts) |
| 5 | "Last used" shown as relative time ("1 min ago"), not a timestamp | owner: admin shouldn't have to check the clock; raw ISO stays in the API (title tooltip) | raw `HH:MM` display |
| 6 | `last_used` window = 24 h | panel answers "recently", older history is the Usage pages | 7 d (noisier, same query cost) |

## Changelog

- 2026-11-21 — owner follow-up (short-request traffic invisible between
  polls): finished streams now stay in the connections panel for 60 s
  (`recentMemory`, bounded by `recentCap`=200, pruned on leave/read).
  `InFlight.Leave` keeps a finished copy with `End`; new `Recent(userID)`
  returns them (active `List` untouched, so counts/busy/utilization are
  unaffected). `connections` rows gained `running` + `ended_at`; dashboard
  dims finished rows and shows "done 4s ago". Tests: recent window/scope/
  cap/double-leave in `inflight_test.go`, e2e finished record, live merge
  of active+recent with running flags and ended_at.
- 2026-11-21 — implemented: `proxy.Stream` records in `InFlight` (`Enter`/
  `EnterAt` return ids, `Leave(id)` exact removal, `List(userID)` sorted
  copies; `Snapshot`/`BusySince` unchanged semantics); `handle()` computes
  the session before entering the tracker (drain refusals never appear as
  connections) and records user/key/model/endpoint/session/ip/streaming;
  `/api/admin/live` gains admin-only `connections` (+`seconds` elapsed) and
  `last_used` (`store.LastUsedByUser`, 24 h, `idx_usage_user_ts`); personal
  `/api/live` omits both keys. Dashboard: admin-only **Active connections**
  and **Last used** panels, relative times (`fmtAgo`/`fmtDurationSec`),
  auto-refresh via the existing 5 s poll. Tests: tracker List/copy/id-Leave,
  e2e record fields during stalled upstream, admin-sees/user-doesn't.
- 2026-11-21 — plan created.
