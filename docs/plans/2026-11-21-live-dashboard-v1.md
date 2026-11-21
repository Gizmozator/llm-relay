# Plan: Live dashboard (default view with utilization graph)

- **Status:** done
- **Owner:** antti
- **Started:** 2026-11-21
- **Issue:** —
- **Supersedes / superseded by:** —

## Goal

After login, every user lands on a **Dashboard** that shows what the relay is
doing *right now*: a live rolling graph of the last hour's traffic (requests,
tokens or cost per minute), how many streams are decoding at this second per
model, and which catalog models are currently online. Admins see the global
view; users see their own traffic. The Usage page (date-range analysis) moves
one nav item down but is otherwise unchanged.

## Non-goals

- No SSE/WebSocket push — the SPA polls; at homelab scale a 5 s poll is
  indistinguishable from push and survives the reverse-proxy path.
- No engine-internal metrics (KV cache fill, per-slot decode speed) — the
  relay only reports what it observes itself.
- No new persistence: live rates come from `usage_log`, in-flight state is
  memory-only and dies with the process.
- No rate limits / concurrency enforcement based on the counters.

## Current state

- `usage_log` rows are inserted **when a request finishes** (buffered
  recorder), indexed on `ts` (UTC RFC3339 text). A finished-stream spike
  therefore lands at completion time — accepted for this view.
- `store.SummarizeUsage` aggregates by day/user/key/... (`substr(u.ts,1,10)`);
  no minute buckets exist. `UsageFilter` has date-granularity `From/To` only.
- The proxy (`internal/proxy`) has **no in-flight tracking** — nothing counts
  active requests today.
- Backend reachability exists only as the admin-triggered full probe
  (`proxy.Probe`: /health + /v1/models, 8 s) stored on the backend row.
- Frontend: Chart.js `LineChart` updates in place (`update('none')`);
  `useLoad` fetches on deps change; index route is the Usage page.

## Approach

- **Minute buckets** — `store.LiveBuckets(db, filter, minutes)`:
  `GROUP BY substr(u.ts,1,16)` over the last N minutes, server-side zero-fill
  so the chart line is continuous. `UsageFilter` gains a `Since` (RFC3339)
  bound; existing `From/To` untouched.
- **In-flight tracker** — `proxy.InFlight`: mutex + `map[(user,model)]int`,
  `Enter/Leave/Snapshot(userID)` (0 = all users).
  Hooked in `Handler.handle` after model resolution
  (deferred Leave), so every metered or forwarded request counts while it is
  upstream. Memory-only by design.
- **Health cache** — `proxy.Reachable(ctx, baseURL)`: GET `/health`, 2 s
  timeout; any HTTP answer counts as online except 503 (llama.cpp-style
  "model not loaded"). `api.Server` caches per-backend results for **60 s**
  (owner decision: models don't flap, 1 min refresh is enough) and refreshes
  synchronously when stale. Model online = model enabled ∧ backend enabled ∧
  backend reachable.
- **Endpoint** — `GET /api/live` (personal scope) and `GET /api/admin/live`
  (unscoped), same scoping rule as usage routes (property of the endpoint).
  Response: `{currency, buckets[], active[], models[]}`. `window` param in
  minutes, default 60, clamped 5–360.
- **Frontend** — new `pages/Dashboard.tsx` on the index route: cards (active
  streams, models online, requests + cost last hour), a metric-switchable
  live LineChart (req/min, tok/min, cost/min), and a model status table.
  Polls every 5 s, paused while the tab is hidden. Usage moves to `/usage`
  with its own nav item.

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | store: `Since` filter + `LiveBuckets` with zero-fill | — | ☑ |
| 2 | proxy: `InFlight` tracker + hook in `handle` | — | ☑ |
| 3 | proxy: `Reachable` health probe | — | ☑ |
| 4 | api: health cache + `Live` handler + routes | 1,2,3 | ☑ |
| 5 | main.go wiring (`Flight` into api.Server) | 2,4 | ☑ |
| 6 | web: api client + Dashboard page + routing/nav | 4 | ☑ |
| 7 | tests: buckets, scoping, inflight (race), health cache | 4 | ☑ |
| 8 | docs: README + this plan's changelog | 6 | ☑ |

## Verification

- [ ] `make ci` green
- [ ] `make web && make build`, log in: index route shows Dashboard; Usage
      still reachable at `/usage` and `/admin/usage` unchanged
- [ ] fire a streaming request through the relay → active-stream count rises
      during the stream, bucket spikes when it completes
- [ ] stop the upstream backend → model flips offline within ~60 s, back
      online after restart within ~60 s
- [ ] user role sees only own buckets/active on `/api/live`; admin
      `/api/admin/live` shows all
- [ ] edge: `window=0` / `window=100000` clamp instead of 500

## Decisions

| # | decision | why | alternatives considered |
|---|---|---|---|
| 1 | 5 s polling, no push | homelab scale; SSE through Cloudflare needs keepalive plumbing and buys nothing here | WebSocket, SSE push |
| 2 | health refresh cached 60 s | owner: models don't flap; keeps the 5 s poll free of upstream probes | probe per poll, reuse last admin probe (too stale) |
| 3 | in-flight counted in memory only | zero schema change; a restart clearing it is correct (streams died too) | periodic flush to SQLite |
| 4 | buckets zero-filled server-side | one source of truth for the window; frontend stays dumb | frontend gap-filling |
| 5 | any HTTP status except 503 = online | 401 means the server is up but keyed (matches plan §7 probe rule) | require 200 |

## Open questions

- (none)

## Changelog

- 2026-11-21 — plan created.
- 2026-11-21 — implemented as planned. Notes: `LiveBuckets` takes the window
  anchor as a parameter (testable clock); the health cache holds its mutex
  across probes, which doubles as probe-stampede dedupe; `InFlight` methods
  are nil-receiver safe so test-built handlers work. `make ci` green;
  endpoint smoke-tested against a running binary (60 buckets, window clamp,
  400 on bad window, SPA served).
- 2026-11-21 — review round 1: `InFlight` keyed by **(user, model)** instead
  of model alone, so `active` on the personal route is the caller's own
  streams (matches the scoping promise); health probe runs on
  `context.WithoutCancel` so a viewer disconnecting mid-probe cannot cache a
  false offline for a minute; `LineChart` now updates the dataset/tooltip
  label on metric switches; added fixed-clock `LiveBuckets` store tests
  (boundaries, zero-fill, scoping) and a canceled-viewer-context test.
