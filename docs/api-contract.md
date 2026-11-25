# llm-relay HTTP API contract (authoritative)

All error responses (any endpoint): `{"error":{"message":"..."}}` with proper status.
Money is always **integer micro-units** (`*_micro`); display value = `micro / 1e6`.
Prices are micro-units **per 1,000,000 tokens**. Timestamps are RFC3339 UTC.
Dates in query params (`from`, `to`) are `YYYY-MM-DD`, inclusive; default range = last 30 days.

## Dashboard API (JWT) — served on the **admin port** (default `:8081`)

All `/api/*` except login require `Authorization: Bearer <jwt>`. The embedded SPA is served from the same port; this is the port to front with the reverse proxy.

### Auth
- `POST /api/auth/login` — body `{"username","password"}` → `200 {"token":"<jwt>","user":{"id":1,"username":"a","role":"admin"}}` · `401` bad creds · `429` (+`Retry-After`) after repeated failures from one client (per-IP token bucket; a successful login resets it)
- `GET /api/me` → `{"id":1,"username":"a","display_name":"Ada","role":"admin"|"user","disabled":false,"created_at":"..."}`
- `PATCH /api/me` — `{"display_name":"..."}` (≤64 chars) → updated user object
- `POST /api/auth/change_password` — `{"old_password","new_password"}` (new ≥8, ≤72 bytes) → `204`; `401` when the old password is wrong, `400` on validation. Changing a password revokes every JWT issued earlier for that user (they must sign in again).
- `GET /api/info` → `{"relay_url":"https://relay.example.fi","dashboard_url":"https://llm-relay.example.fi","currency":"EUR"}` (`currency` defaults to `EUR` when unset; empty `relay_url`/`dashboard_url` = not configured; `relay_url` is the configured `PUBLIC_RELAY_URL` root **verbatim** — the dashboard appends `/v1` for the copy field users paste into their apps. The values are not validated: include the scheme, and a hostname or a plain IP:port origin both work (LAN hosting: `http://192.0.2.10:8080`). When the instance has a self-signed TLS cert, `tls_fingerprint` (`sha256:…`) is added — public-safe (a hash of a public cert), shown so a spoke can pin it; absent when no cert is configured)

### Model catalog (all users)
- `GET /api/models` → `{"models":[{"kind":"model","name":"Qwen3.8-flash-next","price_in":100,"price_out":200,"price_cache_read":10,"price_cache_write":40,"context_length":262144,"concurrency_limit":4,"bench_tok_per_s":41.2,"bench_pp_tok_per_s":1850.4,"bench_at":"2026-11-20T12:00:00Z"}]}` — models usable right now (enabled model on an enabled backend); prices are micro-units per 1M tokens; `context_length`/`concurrency_limit` are admin-set (0 = unset); `bench_tok_per_s` (generation, TG) / `bench_pp_tok_per_s` (prompt processing, PP) / `bench_at` are the last admin benchmark (0/"" = never); either speed may also be set manually by an admin, which leaves `bench_at` empty. Internal wiring (backend, upstream id) is not exposed. `kind` is `"model"` or `"group"`: a group row advertises a pooled public name (several machines behind one name) and carries **zero** numeric fields — consumers must skip group rows for billing or speed display (cost and speed follow the member that actually serves each request).

### API keys (own)
- `GET /api/keys` → `{"keys":[{"id":1,"name":"laptop","prefix":"sk-abcd1234","created_at":"...","last_used_at":"...|null","revoked_at":"...|null"}]}`
- `POST /api/keys` — body `{"name"}` → `201 {"id":2,"name":"phone","key":"sk-<40hex>","prefix":"sk-abcd1234"}` (full key shown ONLY here)
- `DELETE /api/keys/{id}` → `204` (revokes; own keys only)

### Usage (personal endpoints always scope to the caller — admins included; the `/api/admin/usage/*` variants are unscoped and accept `user_id` & `key_id` filters)
Proxy requests may carry `X-Session-Id: <string>`; without it the relay derives a stable session id (hash of key+model+first user message) for conversation-shaped endpoints. Session ids appear on usage rows and as a `group_by` value.
- `GET /api/usage/summary?from&to&model=&group_by=day|user|key|model|ip|endpoint|session&tz_offset=` (default `group_by=day`; `model` = exact `model_requested` match)
  → `{"currency":"EUR","totals":{"requests":0,"prompt_tokens":0,"completion_tokens":0,"cached_tokens":0,"cache_write_tokens":0,"cost_micro":0},"groups":[{"key":"2026-11-20","label":"2026-11-20","requests":0,"prompt_tokens":0,"completion_tokens":0,"cached_tokens":0,"cache_write_tokens":0,"cost_micro":0}]}`
  (`key`/`label`: day → date string; user → username (`(deleted user)` when the account is gone); key → key name, or `federation` (`federation · <peer>` when the row names a hosting peer) for rows that never had a local key — the synthetic federation key, `api_key_id 0` — and `(deleted key)` only when the row's own local key row is gone; model → model name; ip → client IP; endpoint → endpoint id)
  `tz_offset` (optional, integer **minutes east of UTC**, `-1440..1440`; non-integer or out of range → `400`): the viewer's UTC offset. It moves the **day group key** to the viewer's local date (buckets split at the viewer's local midnight instead of UTC) and reads `from`/`to` as **local** dates (local midnight at the offset; the default last-30-days range anchors on the viewer's today). Absent = `0` = plain UTC, exactly the historical behaviour — callers that do not send it are unaffected. The day keys then carry local dates. **Known limitation (fixed-offset approximation):** one offset applies to the whole range — the viewer's offset of *now* — with no DST history per day. `GET /api/usage/detail` accepts the same parameter for consistent range bounds (it has no day grouping).
- `GET /api/usage/detail?from&to&model=&page=1&page_size=50` (`model` = exact `model_requested` match) → `{"total":123,"rows":[{"id":1,"ts":"...","username":"bob","key_name":"laptop","key_local":true,"fed_peer":null,"client_ip":"1.2.3.4","session_id":"a1b2c3d4e5f6","endpoint":"chat","model_requested":"halogen","prompt_tokens":10,"completion_tokens":20,"cached_tokens":0,"cache_write_tokens":0,"cost_micro":300,"streamed":true,"usage_estimated":false,"status_code":200,"latency_ms":1200,"error":null}]}` (newest first)
  `key_local` says whether the row ever referenced a **local** API key: with an empty `key_name` it separates a genuinely deleted key (`key_local:true` — the key row is gone) from federation/spoke origin (`key_local:false` — the synthetic federation key, `api_key_id 0`; the dashboard labels those `federation`, appending ` · <peer>` from `fed_peer`, the hosting peer's name). `fed_peer` is admin-view data (another user's peer is wiring): the personal routes zero it, exactly like `model_upstream`.

### Live dashboard
- `GET /api/live?window=60` and `GET /api/admin/live?window=60` (`window` in minutes, default 60, clamped to 5–360)
  → `{"buckets":[{"minute":"2026-11-21T12:34","requests":0,"prompt_tokens":0,"completion_tokens":0,"cached_tokens":0,"cache_write_tokens":0}],"active":[{"model":"halogen","count":1}],"models":[{"id":1,"name":"halogen","backend":"halogen (iGPU)","online":true,"draining":false,"active":1,"concurrency_limit":4}],"util":[{"model":"halogen","values":[0,12.5],"concurrency_limit":4}]}`
  The live view is **server-wide for every logged-in user** — its job is answering "is the server free" (owner decision); cost is deliberately not exposed here (per-user cost lives on the Usage pages). The admin variant additionally accepts `user_id=<id>` as a drill-down (ignored on the personal route).
  **Admin route only** (absent from the personal response): `connections` — one row per proxied request `{"user","key","model","endpoint","session","ip","streaming","running","started_at","ended_at","seconds"}` — active streams (`running:true`, `seconds` = elapsed run time) plus streams that finished within the last 60 s (`running:false`, `ended_at` set, `seconds` = total duration), so short requests are visible between the 5 s polls; in-memory, oldest start first — and `last_used` — per-user most recent usage row within 24 h `[{"user","last_used"}]`, newest first. The dashboard renders both as relative times ("1 min ago") and refreshes them with the same 5 s poll.
  `buckets`: one entry per minute over the last `window` minutes, zero-filled; a request lands in the minute it **completed** (usage rows are recorded at completion). `active`: in-flight proxied requests per model, all users — in-memory only, cleared by a restart. `models`: enabled catalog models, one row per physical model row with its `id` (group members share a name by design, so consumers must key rows by `id`, not name); `online` = model enabled ∧ backend enabled ∧ backend reachable (GET `/health`, any HTTP answer counts except 5xx — 503 "not ready" and 502/504 from a fronting proxy both mean down; probed at most once per 60 s per backend); `draining` = backend is being taken down (new sessions refused); `concurrency_limit` = that row's slots (0 = unset); `upstream` = engine-side model id, **admin route only** (wiring). On the personal route the model list is group-aware: each ENABLED group with a servable member appears as one synthetic pool row keyed by the **group's** public name (`pool: true`, an explicit marker so consumers need not re-derive it; id = `-groupID`, a synthetic marker that cannot collide with a real model row id; pool-total `active`, summed `concurrency_limit` (0 = unknown), zeroed backend/`online`/`draining`); disabled groups and plain internal (non-member) models are hidden; the pool row **shadows** any same-name public standalone row (the store resolves the name to the group, so the shadowed row is hidden too — the live view matches the catalog); internal member names fold into the group name in `active` and `util` for every group, disabled ones included (their members stay requestable by name), while public member names stay as requested. `util`: per-model utilization — busy % of each minute (`values` aligns with `buckets`), from each request's wall-clock duration distributed across the minutes it occupied, **including streams still running** (their elapsed time is folded in live from the in-flight tracker, so a long request shows busy while it runs, not retroactively at completion); >100 % = parallel streams; `concurrency_limit` (0 = unset) lets the client scale the axis to `limit × 100 %`.

### Agent API (relay port, `sk-` keys)
- `GET /v1/agent-guide` (**no auth**, `text/markdown`) — the integration guide to hand to an AI agent: token creation, MCP config, REST usage, status semantics. The `PUBLIC_RELAY_URL` server root (no `/v1` by convention) and `PUBLIC_DASHBOARD_URL` are interpolated verbatim into the text — whatever origin was configured (hostname or IP:port, scheme included) is what the guide's examples show (unset relay keeps the placeholder visible; unset dashboard drops the mention).
- `GET /v1/utilization` (bearer `sk-` key)
  → `{"window_minutes":5,"models":[{"id":7,"name":"halogen","online":true,"draining":false,"active":1,"concurrency_limit":4,"utilization_pct":37.5,"status":"free"}]}`
  `id` is the physical model row id: same-name rows are distinct members of one model group (several machines behind one public name), so consumers must key rows by `id`.
  Server-wide by design (delegation decisions are global truth). `utilization_pct` credits streams still running (folded from their start time, not waiting for the usage row written at completion). `status`: `offline` (backend unreachable) | `draining` (admin is taking the backend down: new sessions get `503` + `Retry-After: 60`, sessions active within 15 min may continue) | `busy` (`active ≥ concurrency_limit` when set, or mean `utilization_pct ≥` the model's `busy_threshold_pct` over the window; 0 = default 80) | `free`. Visibility: the row list depends on the key's owner. **Non-admin keys receive the advertised catalog** — what `/v1/models` actually serves: one synthetic row per enabled group (name = group name, `id` negative `-groupID` marking the synthetic row, summed member `active` and `concurrency_limit` (0 = unknown), `online` when any servable member's backend answers, `draining` when nothing servable accepts new sessions, `utilization_pct` folding member-keyed busy into the group name; a pool counts `busy` by threshold only when the pooled busy reaches the strictest member threshold — one member below its own threshold keeps the pool readable as not-busy) plus the deduped public rows (enabled model on an enabled backend, same-name rows collapse to the resolver winner, rows shadowed by a group name are dropped). **Admin keys receive the physical rows** — every enabled model including internal group members, keyed by real row id.
- `POST /mcp` (bearer `sk-` key) — stateless MCP over streamable HTTP (JSON-RPC 2.0, plain JSON responses; no sessions/SSE). Methods: `initialize` (version negotiation, `serverInfo`, `instructions`), `ping`, `notifications/*` → `202`, `tools/list`, `tools/call`. One tool: `check_utilization` (optional `model` string argument filters the list; a non-string argument → `-32602`, a name matching no enabled model → in-band `isError`), returning the REST payload as `structuredContent` + text. Unknown method → `-32601`, unknown tool → `-32602`, bad JSON → `-32700`, oversize body (>1 MiB) → HTTP 413, tool *execution* failures are in-band (`isError:true`, generic message — internal errors are logged, not leaked). Any notification (absent `id`), malformed or not, gets `202` with no body.

### Admin — users
- `GET /api/admin/users` → `{"users":[{"id":1,"username":"a","display_name":"Ada","role":"admin","disabled":false,"created_at":"...","price_multiplier":1,"pass_changed_at":"...|"}]}` (`pass_changed_at` = last password change; JWTs issued earlier are revoked; `""` = never)
- `POST /api/admin/users` — `{"username","display_name"?,"password","role":"admin"|"user"}` → `201` user object
- `PATCH /api/admin/users/{id}` — any of `{"password","disabled","role","display_name","price_multiplier"}` → user object. `price_multiplier` (float >0, default 1) scales every cost recorded for that user at request time (snapshot semantics: changing it never rewrites existing rows). Admin-only: `GET /api/me` deliberately omits it. Guard: an update that would leave zero enabled admins → `400`; setting a password >72 bytes → `400`.

### Admin — backends
- `GET /api/admin/backends` → `{"backends":[{"id":1,"name":"halogen","base_url":"http://llm-host:8731","has_key":false,"enabled":true,"draining":false,"online":true,"health_error":"","last_probe_at":"...|null","last_probe_error":"...|null","server_version":"...|null","created_at":"...","model_count":2,"owner_user_id":2|null}]}` — `online` is the **live** reachability verdict (shared health cache, re-probed at most once per minute per backend; disabled backends are offline without a probe) and `health_error` explains a live `false`; `last_probe_*` are the stored result of the last manual Probe only; `owner_user_id` is the fairness-credit owner (ticket #12) — the user credited in the federation ledger when this backend's own hardware serves, `null` when unowned
- `POST /api/admin/backends` — `{"name","base_url","api_key"?,"owner_user_id"?}` → `201` backend object. `owner_user_id` (ticket #12) sets the fairness-credit owner up front; a non-existent user id → `400`
- `POST /api/admin/backends/remote` — `{"name"}` → `201 {"backend":{...},"token":"rb-..."}`: creates a **remote backend** (plan 2026-11-24-remote-backend-v1) — a backend row with `base_url = remote://<id>` and a fresh connect token shown **once** (only its SHA-256 hash is stored). `400` missing name · `409` name taken (one namespace with plain backends)
- `POST /api/admin/backends/{id}/remote/rotate` — replaces a remote backend's connect token (the old one dies with the call, and the live connector channel is dropped) → `200 {"token":"rb-..."}`. `404` unknown id · `400` not a remote backend
- `PATCH /api/admin/backends/{id}` — any of `{"name","base_url","api_key","enabled","draining","owner_user_id"}` (`api_key:""` clears; `owner_user_id:0` clears the fairness owner, a positive id sets it — a non-existent user → `400`) → backend object. On a **remote** row `base_url` edits are refused (`400` — the URL is the registry key), and disabling drops the live channel at once
- `DELETE /api/admin/backends/{id}` → `204` · `409` if catalog models still reference it · a live remote connector channel is dropped
- `POST /api/admin/backends/{id}/probe` → `{"ok":true,"server_version":"0.18.22|","error":"","models":[{"id":"halogen-qwen3.8-flash-next","in_catalog":true,"model_id":3|null}]}` — for a **remote** backend the probe rides the connector's live channel (`GET /health` + `/v1/models` frames answered by the box against its engine); an offline backend fails fast with `ok:false` instead of hanging
- `POST /api/admin/backends/{id}/models` — `{"model_ids":["halogen-qwen3.8-flash-next"]}` → `201 {"created":[{"id":3,"name":"halogen-qwen3.8-flash-next","upstream_model":"halogen-qwen3.8-flash-next"}]}` (skips already-present ids)

### Admin — models (public catalog)
- `GET /api/admin/models` → `{"models":[{"id":1,"name":"halogen","backend_id":1,"backend_name":"halogen","upstream_model":"halogen-qwen3.8-flash-next","visibility":"public","enabled":true,"price_in":100,"price_out":200,"price_cache_read":10,"price_cache_write":0,"context_length":262144,"concurrency_limit":4,"busy_threshold_pct":80,"bench_tok_per_s":41.2,"bench_pp_tok_per_s":1850.4,"bench_at":"...|","fed_visibility":"off","fed_peer_ids":[]}]}`
- `PATCH /api/admin/models/{id}` — any of `{"name","enabled","visibility","price_in","price_out","price_cache_read","price_cache_write","context_length","concurrency_limit","busy_threshold_pct","bench_tok_per_s","bench_pp_tok_per_s","fed_visibility","fed_peer_ids"}` → model object. `visibility` is `"public"` (listed on `/v1/models`) or `"internal"` (hidden from the catalog but still directly requestable); group members are forced `internal` by the store on every membership edit. `bench_tok_per_s`/`bench_pp_tok_per_s` (float ≥0) set the generation/prompt-processing speed manually and clear `bench_at` (configured value, not a measured benchmark). `busy_threshold_pct` (0–10000, else `400`) is the mean-busy % at which the agent surfaces report `busy`; `0` = default 80, values over 100 require that many parallel streams (300 = busy from 3 concurrent). New models are created with `80`. **Federated (materialized) rows** — models on a synthetic `peer://`/`hub://` backend, mirroring a peer's catalog — are **read-only**: any PATCH body is refused with `400 "cannot edit a federated model's settings - managed by the peer"` and nothing is written — including `{"enabled":…}` (a per-model disable would not survive the next catalog push, which re-enables the row) and even `{}` (there is no writable field to mention). The durable kill switches are the peer/backend disable in the Servers section.
- `POST /api/admin/models/{id}/benchmark` — runs two timed **streaming** calls against the upstream. Decode pass (TG): natural-prose prompt; speed = completion tokens over first→last token, so speculative/MTP decoding is measured at realistic acceptance rates. Prefill pass (PP): ~10k-token randomized filler prompt (shrunk to ¾ of `context_length` when that is smaller), `max_tokens=1`; speed = `prompt_tokens` over request→first token (prefill completes before the first token is emitted). May take minutes; `409` while another benchmark runs; `400` for a federated (materialized) model — its "upstream" is a peer relay, not an engine. → `{"bench_tok_per_s":41.2,"completion_tokens":700,"seconds":17.0,"bench_pp_tok_per_s":1850.4,"prompt_tokens":9876,"pp_seconds":5.3,"bench_at":"..."}`. A failed prefill pass never voids the decode result: TG is stored, the previous PP value is kept, and the response carries `"pp_error":"..."` instead of the `bench_pp_tok_per_s`/`prompt_tokens`/`pp_seconds` fields.
- `DELETE /api/admin/models/{id}` → `204`; a federated (materialized) row is refused with `400 "cannot delete a federated model - remove the peer that shares it"` — the row is the peer's mirror, so removal means dropping the peer (Servers page), not deleting the row here.

### Admin — model groups
A group pools several catalog model rows (typically the same model on different backends) under one public name; the proxy resolves the name to one member per session (sessions stay pinned to their member). The group owns its name: it may not collide with a public model name or another group. Members are forced `internal` while they belong to the group and become `public` again when they leave; a member row stays directly requestable either way.
- `GET /api/admin/groups` → `{"groups":[{"id":1,"name":"qwen3-coder","enabled":true,"member_count":2,"members":[{"model_id":3,"weight":41.2,"position":0,"model_name":"qwen3-coder-a","backend_name":"gpu1","upstream_model":"qwen3-coder","visibility":"internal","model_enabled":true,"backend_enabled":true,"backend_draining":false}]}]}`
- `POST /api/admin/groups` — `{"name"}` → `201` group object (empty `members`). New groups are born **disabled**; enabling requires ≥1 member. `400` blank/invalid name · `409` name taken.
- `GET /api/admin/groups/{id}` → group object (same shape as list entries) · `404` unknown id
- `PATCH /api/admin/groups/{id}` — partial merge of any of `{"name","enabled","members"}` → group object. `members` is a **full replacement** list of `{"model_id","weight","position"}` (≤256 entries, else `400`): `weight` 0/omitted seeds from the model's `bench_tok_per_s` (relative share of new sessions); `position` is the member order. `409` when the name would shadow a public model or another group, when a model already belongs to a different group, when the same model is listed twice in `members`, or when enabling a group with no members. A member MAY be a federated (materialized, `peer://`) model — a federated group is exactly a group whose members live on other relays. Such a member is disabled (not removed) while its peer is offline, so the group keeps serving from its remaining members; when the peer stops advertising the model the materializer clears the membership before deleting the row, leaving the group intact.
- `DELETE /api/admin/groups/{id}` → `204` (members are released, not deleted)

### Admin — settings
- `GET /api/admin/settings` → `{"currency":"EUR","retention_days":0}` —
  `retention_days` is the `usage_log` retention window in days, `0` = keep
  forever (plan 2026-11-25-usage-retention-v1; the default).
- `PUT /api/admin/settings` — `{"currency":"USD"}` → same shape.
  `retention_days` is optional (absent = unchanged; `0` or `30..3650`,
  anything else → `400`). When present it is stored and a bounded purge runs
  in the request: rows older than the new cutoff go immediately, a larger
  backlog is left to the daily pass (a startup + 24 h loop enforces the
  setting with batched short-transaction deletes).

### Federation (plan 2026-11-22-relay-federation, task 1)
Instance role + spoke config, and the peer registry (server tokens a spoke
dials the hub with). Server tokens are `fed-…` strings, stored hashed like
API keys, shown **only** in the create/rotate response. Disabling the
owner user revokes their tokens immediately (same contract as API keys).
Type scoping is
structural: `sk-` keys authenticate `/v1/*` only, `fed-` tokens the
`/federation/*` channel only (separate tables; both directions tested).

Self-service (JWT, any user — the **My Keys** surface):
- `GET /api/peers` → `{"peers":[{"id":1,"name":"home-server","owner_user_id":2,"owner_name":"alice","enabled":true,"last_seen":"","version":"","created_at":"...","shared_models":3}]}` (own peers only)
- `POST /api/peers` — `{"name"}` → `201 {"peer":{…},"token":"fed-…"}` — the raw token appears exactly here
- `POST /api/peers/{id}/rotate` (own only, else `404`) → `{"token":"fed-…"}`; the old token dies immediately, and so does any live federation channel riding it (close `1008 "rotated"`)
- `DELETE /api/peers/{id}` (own only, else `404`) → `204`; a live channel is dropped too (`1008 "deleted"`)

Admin (JWT, admin — the **Servers** page):
- `GET /api/admin/peers` → all peers, same shape plus `online` (live WS channel at this hub; always `false` on a standalone/spoke box) (owner name + `shared_models` = models visible to that peer: every `fed_visibility:"all"` model + its allowlist rows)
- `POST /api/admin/peers/{id}/rotate` → `{"token":"fed-…"}` · `PATCH /api/admin/peers/{id}` — `{"enabled":true|false}` → peer object (disable is the incident lever: it drops the peer's live channel at once, `1008 "disabled"`) · `DELETE /api/admin/peers/{id}` → `204` (live channel dropped too) — all `404` on unknown id

Federation settings (JWT, admin):
- `GET /api/admin/federation` → `{"role":"standalone","hub_url":"","hub_token":"","hub_pin":""}` — `role` is `standalone` (default) | `hub` | `spoke`; `hub_token` is echoed back (it is the hub's token the admin pasted, not a secret this server minted). Spoke config is plaintext-at-rest like other DB secrets here; the spoke must present it verbatim. `hub_pin` (plan 2026-11-24-remote-tls-v1) is the optional hub cert fingerprint the spoke pins — public data, echoed for review. When the stored role is `spoke` the response carries `"spoke":{"connected":bool,"hub_name":"…","hub_version":"…"}` — the live channel state and the hub's `hello_ack` identity (empty strings while disconnected).
- `PUT /api/admin/federation` — full replacement `{"role","hub_url","hub_token","hub_pin"}` → same shape. `400` when the role is not one of the three, or a spoke lacks an **https** `hub_url` (federation requires TLS — no cleartext, no opt-out) or the `hub_token`; a malformed `hub_pin` (not `sha256:` + 64 hex, colons/case tolerated) is `400`. Leaving spoke mode clears `hub_url`/`hub_token`/`hub_pin`. Live-safe: the spoke dialer starts/stops with the role, and moving away from `hub` closes every live channel (`1008 "role changed"`) — no restart needed.
- `GET /api/admin/tls` → the instance TLS cert view (plan 2026-11-24-remote-tls-v1): `{"configured":false}` when none, else `{"configured":true,"fingerprint":"sha256:…","sans":["relay.example.fi"],"not_after":"…","created_at":"…"}`. The private key is **never** returned.
- `POST /api/admin/tls` — `{"hosts":["relay.example.fi","192.0.2.10"]}` mints (or regenerates) a self-signed cert for those SANs → `200 {"tls":{...view},"cert_pem":"-----BEGIN CERTIFICATE-----…"}` (`cert_pem` is shown once, for an optional install in an external proxy). `400` no valid host. Regenerating replaces the key pair: live channels survive, every peer must re-pin before reconnecting.
- `DELETE /api/admin/tls` → removes the row → `200 {"configured":false}`. TLS handshakes fail until a new cert exists (deliberate — no silent fallback).
- `GET /api/federation/role` (JWT, any user) → `{"role":"hub"}` — the reconciled role only; the hub-only UI (setup guide) gates on it without needing the admin settings payload.

Federation stats (plan task 4; fairness scope per plan
2026-11-25-fairness-ledger). Window: `?days=N` (default 30, `1..365` else
`400`) = rolling last-N-days with an open upper bound, or `?month=YYYY-MM`
(a UTC calendar month, `[first day, first day of next)`; malformed → `400`;
`month` wins when both are present). The response echoes `since` and
`until` (`until` is `""` for rolling windows).

- `GET /api/admin/federation/stats` (JWT, admin) — the fairness ledger,
  **role-dependent** payload:
  - hub (the fairness ledger — every request on capacity owned by someone
    else is double-entered as the host's `served` and the spender's
    `consumed`, whichever transport it took):
    `{"role":"hub","since":"…","until":"","users":[{"user_id":4,"user":"alice",
    "served":{…},"consumed":{…}}],"peers":[{"peer_id":3,"peer":"home-server",
    "served":{…},"consumed":{…}}],"local_only":[{"user_id":7,"user":"bob",
    "counts":{…}}]}` — user entries are keyed by `user_id`
    (deleted users share the `"(deleted user)"` label but stay distinct
    entries); a peer's `served` sums the rows naming it as host, its
    `consumed` is owner-level (every consumed row of the peer's owner —
    an owner with several peers carries the same total under each).
    Three passes feed the per-user table: (1) cross-peer flows — each wrote
    a `consumed` row on the spender and a `served` row on the hosting peer's
    owner; (2) **hub-hardware serving credited through a backend owner**
    (ticket #12): a federated row served by this relay's own hardware
    (`fed_peer_id` NULL) whose serving model's backend has an
    `owner_user_id` credits that owner with the serving; (3) **local
    cross-user traffic** (plan 2026-11-25-fairness-ledger): a plain
    (`fed_direction` empty) row whose backend has an `owner_user_id` other
    than the consuming user credits the owner with serving and charges the
    user's account as consumed — so "account A's hosted models being used by
    account B" appears in the ledger whether B dialed the hub directly or
    arrived via B's spoke. Unpairable rows stay out: self-use (owner ==
    consumer), ownerless backends, and anonymous rows (`user_id` NULL/0).
    All attribution is a query-time join (`usage_log → models →
    backends.owner_user_id`), so it covers historical rows the moment an
    owner is set; the local pass is disjoint from the fed passes by
    `fed_direction`, and the hub-owner credit lands on the serving side of
    rows the first pass books as consumed, so no bucket is ever inflated. `local_only` (ticket #13, narrowed by
    2026-11-25-fairness-ledger) names the accounts with window usage the
    ledger cannot show at all — self-use or ownerless-backend usage only, no
    federation rows; anyone the ledger shows (either side of any pass) is
    dropped from it. An empty `users` **and** empty `local_only` means there
    was no usage at all. The UI uses this to say "nothing to attribute
    between accounts" instead of showing a blank that reads as missing data.
  - spoke: `{"role":"spoke","since":"…","until":…,"served_by_source":[{"fed_user":
    "hub-alice","counts":{…}}],"consumed":[{"user_id":4,"user":"alice",
    "counts":{…}}]}` — what this box served to which hub-stamped
    identity, and what local users spent on federated
    (synthetic-backend) models.
  - standalone: `{"role":"standalone"}`.
  - Every `{…}` counts bucket is `{"requests","prompt_tokens",
    "completion_tokens","cached_tokens","cache_write_tokens","cost_micro"}`.
- `GET /api/federation/stats` (JWT, any user) — the caller's own fairness
  totals, always scoped to the caller (admins included): hub → `{"role":"hub",
  "since":…,"until":…,"totals":[one FedUserTotal],"local_only":[caller's entry
  or empty]}` — the caller's served (their hosted models serving other
  accounts, local and federated alike) and consumed (their spending on others'
  hardware); `local_only` carries their unattributable usage when the ledger
  has no entry for them (the personal "nothing to attribute" signal,
  ticket #13); spoke → `{"role":"spoke",
  "since":…,"until":…,"consumed":[caller's slice]}` (served rows belong to the box,
  not to a session user); standalone → `{"role":"standalone"}`.

Per-model sharing switch (rides the model object everywhere the admin API
returns models): `PATCH /api/admin/models/{id}` gains `fed_visibility`
(`"off"` default | `"all"` | `"allowlist"`, else `400`) and `fed_peer_ids`
(replaces the peer allowlist wholesale; only accepted together with
`fed_visibility:"allowlist"`, unknown peer id → `400`); on a federated
(materialized) row the pair is peer-owned, so mentioning it is a `400` (see the
`PATCH /api/admin/models/{id}` line). PATCH semantics:
switching to `"allowlist"` without `fed_peer_ids` keeps the stored list;
switching to `"off"`/`"all"` clears it (those modes share with
nobody/everybody). The model row surfaces both as `fed_visibility` +
`fed_peer_ids`.

**TLS gate (both channels, plan 2026-11-24-remote-tls-v1):** the three
machine-channel endpoints — `/federation/v1/connect`, `/federation/v1/ping`
and `/remote/v1/connect` — answer `403 "TLS required"` to any request that did
not arrive over TLS: a direct TLS handshake (`r.TLS`), or `X-Forwarded-Proto:
https` when `TRUST_PROXY_HEADERS` is on **and `TLS_PORT=0`** (the
TLS-terminating-proxy deployment — a relay that binds its own TLS listener does
not honour the forgeable header). No opt-out. The dialers require TLS too: a
spoke's `hub_url` and a connector's `RELAY_URL` must be `https://`, and against
a self-signed peer they pin its fingerprint (`hub_pin` / `RELAY_PIN`).

Federation channel (relay port, `fed-` tokens):

- `GET /federation/v1/ping` — token-path probe behind the server-token
  middleware: `200 {"pong":true,"peer_id":1,"peer_name":"home-server"}` ·
  `401` missing/unknown token · `403` peer disabled.
- `GET /federation/v1/connect` — the WebSocket channel (plan
  2026-11-22-relay-federation, task 2). The upgrade request carries the
  `fed-` token as `Authorization: Bearer` (same `401`/`403` taxonomy as
  ping). **Role gate:** unless this instance is `hub`, the endpoint
  answers `404` (standalone/spoke accept nothing). After the upgrade the
  spoke must send `hello {proto:1,name,version}` within **5 s**, else the
  socket closes `1008 "hello required"`; a foreign `proto` is refused
  with close `1002 "proto mismatch"`. The hub answers
  `hello_ack {proto:1,name,version}` and records the spoke's version.
  A second connect with the same token **replaces** the live channel
  (old socket closed `1008 "replaced"`).
- Frame envelope: JSON text frames `{"t":<type>,"id"?,"...payload"}`,
  read limit 4 MiB/frame. Types: `hello`, `hello_ack`, `ping`, `pong`,
  `catalog`, `status`, `req`, `res`, `res_body`, `res_end`, `cancel`
  (unknown types are ignored for forward-compat). Binary payloads
  (`req.body`, `res_body.data`) ride base64 inside the JSON.

Remote-backend channel (relay port, `rb-` tokens, plan
2026-11-24-remote-backend-v1):

- `GET /remote/v1/connect` — the WebSocket channel a **remote backend**
  (connector box) dials in on. **Not role-gated**: any relay role accepts
  remote backends. The upgrade request carries the `rb-` connect token as
  `Authorization: Bearer`: `401` missing/unknown token · `403` backend
  disabled. After the upgrade the connector sends
  `hello {proto:1,name,version}` within **5 s** (same close codes as the
  federation channel); the relay answers `hello_ack` and registers the
  channel under the backend id. A second connect with the same token
  **replaces** the live channel. Frames: the same fed envelope and types as
  above minus `catalog`/`status` — the relay sends `req` (paths restricted
  to `/v1/*` and `/health`), the connector answers `res`/`res_body`/
  `res_end` from its local engine and honors `cancel`. Backend health =
  the live channel; token rotate / disable / delete drop it immediately.
- Liveness (both sides): `ping` every **20 s**; the channel must show any
  inbound frame within **10 s** of it or it closes (`1008 "pong
  timeout"`). The hub refreshes the peer's `last_seen` at most once per
  **30 s**. A spoke that drops reconnects with jittered exponential
  backoff 1 s→60 s (reset after 60 s stable). Role changes via
  `PUT /api/admin/federation` take effect live (no restart).
- Revocation: disable/rotate/delete on the peer endpoints drop the live
  channel immediately. Changes that do not pass those endpoints (e.g. the
  owner disabled via the admin users API) are caught by the hub's
  revalidation sweep — the **30 s** touch slot also re-checks the peer
  row; gone, disabled or owner-disabled closes the channel
  (`1008 "revoked"`), so revocation lands within ~30 s either way. A
  spoke reconnects only with a valid token: after a rotate, the old
  channel stays dead until the spoke's config carries the new token.

#### Catalog sync (task 3)

- `catalog {origin?,models,groups}` — the sender's shared catalog.
  `origin` labels whose entries these are: `0`/absent = the sender's own;
  a peer id = another spoke's entries relayed by the hub. `models` and
  `groups` are **always serialized**, and null vs empty differ in meaning:
  `null`/absent = "this frame carries no data for that section" (partial
  or malformed frame — the receiver skips it, never prunes); `[]` = "shares
  nothing" and does prune the materialized rows of that section. A model
  entry carries `name`, `upstream_model`, the four prices, `context_length`
  and the bench/concurrency meta; a group entry carries `name` plus members
  as `{model_name,weight}` — **names only, ids never cross the wire**.
- A spoke pushes its catalog right after `hello_ack` and after every
  debounced (**500 ms**) local change to model sharing/enabled, backend
  enabled or groups. Only `fed_visibility='all'` models are shared from a
  spoke: `allowlist` names rows of *this* instance's peer table, which a
  spoke does not have — it shares nobody.
- The hub answers each spoke with the **merged catalog**: every other
  spoke's relayed entries (one frame per origin, `origin=<peer id>`)
  first, then its own frame (`origin:0`) — its `SharedWithPeer`-filtered
  models plus its enabled groups, each pre-filtered to members visible to
  that peer (a group with no visible member is not named at all).
- `status {peer_id,online}` — the hub broadcasts peer liveness on connect
  and channel death; a spoke maps it onto that peer's materialized
  backend (offline = disabled backend, rows kept so model ids survive the
  flap). Direction is **hub→spoke only**: a spoke cannot push status (it
  does not know its own peer id, and a peer that could name a victim could
  flip another peer's backend at the hub), so hub channels ignore inbound
  status frames.

#### Materialization

A peer's catalog becomes ordinary local rows: models on a **synthetic
backend** whose `base_url` is `peer://<peer-id>` (or `hub://federation` on
a spoke, for the hub's own entries), and, on a spoke, the hub's groups as
`model_groups` rows marked `federated=1`. From there HRW affinity,
health, drain, `/v1/models` and metering treat them like local rows.
Rules: upsert by `(backend,name)` keeps row ids stable across catalog
churn; a frame prunes what it no longer advertises (subject to the
null-vs-empty rule above — a section with no data never prunes); a peer
delete removes its backend and rows; startup and role transitions purge
all synthetic rows (they are rebuilt from live frames), and the purge also
returns federated-group members to `public` — leaving the federation
undoes what a federated group did to a local model. An **enabled** edit on
a materialized model row is not durable: the next catalog push re-enables
it (the upsert update sets `enabled=1`, since the peer still advertises
the model) — so the admin API refuses the edit outright; the durable kill
switches are the peer disable and the synthetic-backend disable. Synthetic backends are
never HTTP-probed — their health **is** the channel — and the admin probe
and benchmark endpoints refuse them (`400`); the model edit and delete
endpoints refuse the materialized **rows** the same way (they are
read-only — even `enabled` is the peer's — and removal is per-peer).
Collision policy: a shared name owned by a local group or a local
(non-synthetic) public model is **skipped with a logged warning** (the
admin renames on their side); two peers advertising the same name is fine
(distinct backends). Group members resolve local-first: a spoke with its
own copy of a member model hashes its own sessions to it — and stays that
member across re-pushes (a member row's membership in the very federated
group being re-applied never disqualifies it). A catalog frame
is dropped (with a log line) when it arrives on a channel the manager no
longer trusts — role changed since the channel opened, or the channel was
replaced — so a dying channel's queued frames cannot re-materialize rows
a role transition just purged.

#### Request tunnel

- `req {id,target?,origin?,fed_user?,method,path,headers,body}` — one
  tunneled request; `target:0` (or absent) means "serve here",
  `target=<peer-id>` means "transit to that spoke" (spoke→spoke via the
  hub; never routes a frame back to its origin — loop guard). `origin` is
  **receiver-derived, never sender-trusted**: each relay labels a request
  with the peer id of the channel it arrived on (a spoke always arrives as
  `0` to its hub; the hub stamps the requesting peer on transit), so a
  spoke cannot claim another peer's id to inherit its sharing rights.
  `fed_user` is the **hub-stamped hub-side identity** on whose behalf the
  request rides: a hub-direct consumer's own username, or the origin
  peer's owner name on transit — a spoke sends nothing (spoke-local
  usernames never leave the spoke), and a serving spoke records it on its
  served usage row.
  `res {id,status,headers}` / `res_body {id,data}`
  / `res_end {id,error?}` stream the answer; `cancel {id}` aborts it.
  Bodies are capped at **3 MiB − 4 KiB** raw per frame (base64 stays
  under the 4 MiB read limit); an oversized request fails as a dial
  error. At most **16** concurrent tunneled requests per peer channel —
  the cap surfaces as a dial failure, which the group retry loop already
  treats as failover.
- Outbound: a `peer://`/`hub://` backend is dialed through the channel by
  the proxy's scheme-dispatching transport; auth headers and hop-by-hop
  framing are stripped at the hop, `X-Session-Id` survives (affinity
  crosses the tunnel), and the model name on the wire stays the *catalog
  name* — the serving relay applies its own upstream mapping locally.
- Serving side: the hub serves a spoke's request as the peer's **owner
  user** with a synthetic `federation` key (`api_key_id=0`; the row is
  stamped `consumed`, see Metering below) and enforces the sharing rules —
  a model not shared with the requesting peer answers `403 {"error":
  {"message":"model \"x\" is not shared with this peer"}}`, and group
  members invisible to the peer are dropped before ranking. A spoke serving
  the hub uses local engines only (never re-forwards), applies its own
  stored backend keys, meters the request as a `served` row (see Metering
  below), and enforces **its own** sharing rules on the hub's requests
  too: a model with `fed_visibility='off'` (or `allowlist` — a spoke shares
  nobody by name) answers the same `403`, so a compromised hub cannot name
  a private spoke model into serving. Only `/v1/*` paths are
  served over the tunnel.

#### Metering and attribution (task 4)

`usage_log` carries three federation columns (migration 0013):
`fed_direction` (`''` = plain local traffic | `served` = this relay's
capacity went to a peer | `consumed` = this relay's account used a peer's
capacity), `fed_peer_id` (**always the hosting peer** — the peer whose
hardware served — `NULL` when hub hardware served or the row sits on a
spoke with no local peer row), and `fed_user` (the hub-stamped hub-side
identity a serving spoke sees — spoke-local usernames never travel). The
hub's ledger is **fully double-entry**: every hub row whose resolved
upstream member lives on a synthetic `peer://` backend is mirrored by a
`served` row crediting that backend's owner user with the same token/cost
counts and the same model row (synthetic federation key, `api_key_id=0`).
Because `api_key_id = 0` is exactly "never had a local key", the usage
detail (`key_local`) and the `group_by=key` bucket label use it to tell a
deleted key (a non-zero id whose key row is gone) from federation origin.
Uniform rule: a `served` row credits the host (`fed_peer_id` = host); a
`consumed` row identifies the consumer by its `user_id` and names the
host it spent on (`NULL` when hub hardware served). Who stamps what, per
flow:

- **hub user → spoke-hosted model** (direct or via a group): the
  client's ordinary row gains `consumed` + `fed_peer_id` = the hosting
  peer (the resolved member decides — a group that fell over onto a local
  member stays plain), and the hub mirrors a `served` row for the
  hosting peer's owner (`fed_peer_id` = the same host, `fed_user` = the
  consuming hub username); the spoke also records its own `served` row
  (federation identity, `fed_user` = the hub client's username).
- **spoke user → hub-local model**: the spoke meters its own user as plain
  local traffic (the consumed slice is identified by the `hub://` backend
  scheme, not by fed columns — no double-stamp); the hub writes `consumed`
  only, on the peer's owner, `fed_peer_id` NULL (hub hardware served — no
  peer to credit).
- **spoke A → hub serving a model hosted on spoke B**: the hub's row is
  `consumed` on A's owner with `fed_peer_id` = the **hosting** peer B,
  plus the mirrored `served` row (B's owner, `fed_peer_id` = B,
  `fed_user` = A's owner name); A meters its local user plainly and B
  writes its own `served` row with the hub-stamped `fed_user`.
- **spoke A → spoke B (transit)**: A meters its local user plainly (the
  `peer://` row); the hub pumps the frames outside the pipeline and
  writes the same pair manually — `consumed` on A's owner
  (`fed_peer_id` = B) and `served` on B's owner (`fed_peer_id` = B,
  `fed_user` = A's owner name), counts parsed from the response frames;
  transit never enters the pipeline's metering path, so the two cannot
  double-write. B also writes its own `served` row with the hub-stamped
  `fed_user`.
- **plain local traffic** keeps `fed_direction=''` — untouched rows.

Costs on hub-side federation rows are computed from the model's own
pricing (transit rows without a user multiplier — the ledger compares
capacity, not billed money). A hub-hardware serve (a `consumed` row with
`fed_peer_id` NULL) has no hosting peer to credit, so the aggregation derives
its `served` side from the serving backend's `owner_user_id` when one is set
(ticket #12) — see the "Federation stats" note above. The aggregation queries
live in `internal/store/fedstats.go`; the HTTP shape is under "Federation
stats" above.


Admin usage = same summary/detail endpoints under `/api/admin/usage/...` with extra optional `user_id`, `key_id` params; detail rows always include `username`.

## Proxy API (API keys) — served on the **relay port** (default `:8080`)

`Authorization: Bearer sk-...` · `401` unknown/revoked key · `404` unknown or disabled model (OpenAI-style error).
Routes: `POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/responses`, `POST /v1/messages`, `POST /v1/messages/count_tokens` (pass-through, not logged), `POST /v1/embeddings`, `GET /v1/models`.
**Drain mode**: while the backend serving the requested model has `draining` set, requests starting a *new* session (session id = `X-Session-Id` header or the derived conversation hash; empty counts as new) are refused with `503` + `Retry-After: 60`; sessions active within the last 15 minutes (in-memory tracker, `usage_log` fallback after a relay restart) pass through — graceful shutdown for running conversations.
`GET /v1/models` → `{"object":"list","data":[{"id":"<public name>","object":"model","owned_by":"llm-relay"}]}`

## Misc
- `GET /healthz` → `200 {"status":"ok"}` (no auth)
- SPA served at `/` (index.html fallback for client-side routes).
