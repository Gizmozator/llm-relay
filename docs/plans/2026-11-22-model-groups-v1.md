# Plan: Model groups — session-affine load balancing across replicas

- **Status:** active
- **Owner:** maintainer
- **Started:** 2026-11-22
- **Issue:** #1
- **Supersedes / superseded by:** —

## Goal

An admin can create a *model group*: a public name (e.g. `qwen3-coder`) backed by
several model rows on different backends. Requests for the group name are
distributed across the members with **session affinity** — the same conversation
always lands on the same member — so each engine's KV/prompt cache stays warm.
Members stay directly requestable (pin/debug), and per-member and per-pool usage
are visible in the dashboard.

## Non-goals

- No priority/standby strategy (sticky-hash only), no nested groups, no
  auto-grouping from discovery, no per-member overrides inside a group.
- No saturation-based rerouting: a hashed member stays chosen even when busy
  (engine queues); `concurrency_limit`/`busy_threshold_pct` stay dashboard-only.
- No federation (that is #2, blocked by this plan).

## Current state

- `models` (0001_init.sql): `name TEXT NOT NULL UNIQUE`, `UNIQUE(backend_id,
  upstream_model)`, one `backend_id` FK per row. `store.ModelByName` returns
  `(*Model, *Backend)`; proxy `handle()` forwards to `backend.BaseURL`.
- Session key exists pre-routing: `X-Session-Id` header or `DeriveSession`
  (hash of key+model+first user msg) — proxy.go.
- Drain gate in `handle()` 503s new sessions on a draining backend;
  `HealthCache` caches backend reachability; `InFlight` tracks active streams.
- `/v1/models` lists `store.ListPublicModels` (enabled only).
- Admin API routes are registered in `cmd/llm-relay/main.go` as
  `mux.Handle("GET /api/admin/models", s.Admin(...))` etc.; handlers in
  `internal/api/admin.go`; web dashboard pages in `web/src/pages/`.
- Migrations: `internal/db/migrations/NNNN_name.sql`, applied in order
  (latest: 0009). SQLite: dropping a column UNIQUE requires a table rebuild.

## Approach

- **Migration 0010**: rebuild `models` — drop `name` column UNIQUE, add
  `UNIQUE(backend_id, name)` and `visibility TEXT NOT NULL DEFAULT 'public'`
  (`public`|`internal`). New tables `model_groups(id, name UNIQUE, enabled)`
  and `model_group_members(group_id, model_id UNIQUE, weight REAL, position
  INTEGER)`. `usage_log` untouched.
- **Resolution** (`store`): public-name lookup order = enabled group by name →
  else model by name preferring `public`, tie broken by lowest id (same-name
  members across backends are addressable only when renamed; the group owns the
  shared name). Group names must not collide with any public model name or
  another group (checked in store).
- **Member pick** (`internal/proxy`, new `groups.go`): rendezvous (HRW) hash —
  `u = fnv/xxh(session+"\x1f"+backend_id) normalized to (0,1]`,
  `score = weight / -ln(u)`; walk the ranked list, skipping hard exclusions:
  disabled member/backend, backend draining, backend failing `HealthCache`.
  Empty session id → least-loaded member by `InFlight` count. All members
  excluded → same 503 shape as today's drain gate.
- **Proxy**: group request resolves to a concrete member `(model, backend)` and
  the existing path (rewrite, drain gate for the *chosen* backend, SSE, metering)
  runs unchanged; `DeriveSession` is called with the **requested** name so the
  session is stable across membership edits. Pre-first-byte retry: on dial/connect
  error before any response byte, try the next member in the ranking (group
  requests only).
- **`/v1/models`**: enabled groups + enabled `public` models; `internal`
  members hidden but requestable.
- **Admin API**: `GET/POST /api/admin/groups`, `GET/PATCH /api/admin/groups/{id}`
  (members: model_id, weight, position; weight seeded from `bench_tok_per_s`),
  `DELETE /api/admin/groups/{id}`, `PATCH /api/admin/models/{id}` gains
  `visibility`. Adding a model to a group sets it `internal` (admin may re-expose).
- **Web**: admin Models page gains a groups section (create/edit members,
  weights, enable); model rows show visibility + group membership; user Models
  page lists groups; Usage page: per-pool aggregation by `model_requested`.
- **Docs**: README (groups + routing behavior), AGENTS.md only if layout/entry
  points change; this plan updated to reality in the final commit.

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | migration 0010 + store CRUD + resolution + unit tests | — | ☐ |
| 2 | proxy HRW pick + exclusions + retry + tests | 1 | ☐ |
| 3 | admin API (groups CRUD, visibility) + tests | 1 | ☐ |
| 4 | web UI (groups editor, visibility, usage aggregation) | 3 | ☐ |
| 5 | docs + plan update + live smoke | 2,4 | ☐ |

## Verification

- [ ] `make ci` green at every commit
- [ ] unit: two backends, same model name, import without collision; group
      routes across both; same session → same member across N requests;
      different sessions spread ~proportional to weights
- [ ] unit: draining/unhealthy/disabled member skipped; all-draining → 503
- [ ] unit: `/v1/models` shows group, hides internal members; member name still
      resolves directly
- [ ] unit: usage row has member `model_id` + group `model_requested`
- [ ] live smoke: binary + two mock engines (httptest or `nc`), curl with
      `X-Session-Id`, verify affinity + dashboard rows

## Decisions

| # | decision | why | alternatives considered |
|---|---|---|---|
| 1 | groups as separate tables, `models` untouched except rebuild | additive; per-machine tracking free via member `model_id` | replica join table under a logical model (migratory, hides machine identity) |
| 2 | weighted rendezvous hash `w/-ln(u)` | stateless, restart-stable, minimal churn | modulo (full remap), persisted mapping table (state+GC) |
| 3 | saturation does not reroute | cache warmth is the point | overflow to next member (rejected by owner) |
| 4 | group name shadows same-name members | group owns the public name | member-first (breaks pool semantics) |
| 5 | retry only pre-first-byte | tokens already spent mid-stream must not double-bill | full request retry (unsafe) |

## Open questions

- None blocking. (Live-view member display already works: in-flight `Stream.Model`
  is the resolved member name.)
- Live page keys active counts and `concurrency_limit` by model **name**
  (`internal/api/live.go`): same-name duplicates across backends aggregate into
  one entry (consistent with name-based metering; benign pre-grouping window),
  but per-row limit divergence picks one row's limit for the utilization axis.
  Display nuance — task 4's pool aggregation revisits this.
- Admin model list (`store.ListModels`) deliberately returns **every** row
  (replicas are per-backend admin state; dedupe would hide them) — the web UI
  must key model rows by id, not name.

## Changelog

- 2026-11-22 — plan created from Gitea #1 design.
- 2026-11-22 — task 3: group update endpoint is `PATCH /api/admin/groups/{id}`
  (partial merge), not `PUT` — repo convention reserves `PUT` for full
  replacement (settings); every sibling partial-update admin route uses PATCH.
