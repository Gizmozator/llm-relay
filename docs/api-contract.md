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
- `GET /api/info` → `{"relay_url":"https://relay.example.fi/v1","dashboard_url":"https://llm-relay.example.fi","currency":"EUR"}` (`currency` defaults to `EUR` when unset; empty `relay_url`/`dashboard_url` = not configured; `relay_url` is what users put in their apps)

### Model catalog (all users)
- `GET /api/models` → `{"models":[{"kind":"model","name":"Qwen3.8-flash-next","price_in":100,"price_out":200,"price_cache_read":10,"price_cache_write":40,"context_length":262144,"concurrency_limit":4,"bench_tok_per_s":41.2,"bench_pp_tok_per_s":1850.4,"bench_at":"2026-11-20T12:00:00Z"}]}` — models usable right now (enabled model on an enabled backend); prices are micro-units per 1M tokens; `context_length`/`concurrency_limit` are admin-set (0 = unset); `bench_tok_per_s` (generation, TG) / `bench_pp_tok_per_s` (prompt processing, PP) / `bench_at` are the last admin benchmark (0/"" = never); either speed may also be set manually by an admin, which leaves `bench_at` empty. Internal wiring (backend, upstream id) is not exposed. `kind` is `"model"` or `"group"`: a group row advertises a pooled public name (several machines behind one name) and carries **zero** numeric fields — consumers must skip group rows for billing or speed display (cost and speed follow the member that actually serves each request).

### API keys (own)
- `GET /api/keys` → `{"keys":[{"id":1,"name":"laptop","prefix":"sk-abcd1234","created_at":"...","last_used_at":"...|null","revoked_at":"...|null"}]}`
- `POST /api/keys` — body `{"name"}` → `201 {"id":2,"name":"phone","key":"sk-<40hex>","prefix":"sk-abcd1234"}` (full key shown ONLY here)
- `DELETE /api/keys/{id}` → `204` (revokes; own keys only)

### Usage (personal endpoints always scope to the caller — admins included; the `/api/admin/usage/*` variants are unscoped and accept `user_id` & `key_id` filters)
Proxy requests may carry `X-Session-Id: <string>`; without it the relay derives a stable session id (hash of key+model+first user message) for conversation-shaped endpoints. Session ids appear on usage rows and as a `group_by` value.
- `GET /api/usage/summary?from&to&model=&group_by=day|user|key|model|ip|endpoint|session` (default `group_by=day`; `model` = exact `model_requested` match)
  → `{"currency":"EUR","totals":{"requests":0,"prompt_tokens":0,"completion_tokens":0,"cached_tokens":0,"cache_write_tokens":0,"cost_micro":0},"groups":[{"key":"2026-11-20","label":"2026-11-20","requests":0,"prompt_tokens":0,"completion_tokens":0,"cached_tokens":0,"cache_write_tokens":0,"cost_micro":0}]}`
  (`key`/`label`: day → date string; user → username (`(deleted user)` when the account is gone); key → key name; model → model name; ip → client IP; endpoint → endpoint id)
- `GET /api/usage/detail?from&to&model=&page=1&page_size=50` (`model` = exact `model_requested` match) → `{"total":123,"rows":[{"id":1,"ts":"...","username":"bob","key_name":"laptop","client_ip":"1.2.3.4","session_id":"a1b2c3d4e5f6","endpoint":"chat","model_requested":"halogen","prompt_tokens":10,"completion_tokens":20,"cached_tokens":0,"cache_write_tokens":0,"cost_micro":300,"streamed":true,"usage_estimated":false,"status_code":200,"latency_ms":1200,"error":null}]}` (newest first)

### Live dashboard
- `GET /api/live?window=60` and `GET /api/admin/live?window=60` (`window` in minutes, default 60, clamped to 5–360)
  → `{"buckets":[{"minute":"2026-11-21T12:34","requests":0,"prompt_tokens":0,"completion_tokens":0,"cached_tokens":0,"cache_write_tokens":0}],"active":[{"model":"halogen","count":1}],"models":[{"id":1,"name":"halogen","backend":"halogen (iGPU)","online":true,"draining":false,"active":1,"concurrency_limit":4}],"util":[{"model":"halogen","values":[0,12.5],"concurrency_limit":4}]}`
  The live view is **server-wide for every logged-in user** — its job is answering "is the server free" (owner decision); cost is deliberately not exposed here (per-user cost lives on the Usage pages). The admin variant additionally accepts `user_id=<id>` as a drill-down (ignored on the personal route).
  **Admin route only** (absent from the personal response): `connections` — one row per proxied request `{"user","key","model","endpoint","session","ip","streaming","running","started_at","ended_at","seconds"}` — active streams (`running:true`, `seconds` = elapsed run time) plus streams that finished within the last 60 s (`running:false`, `ended_at` set, `seconds` = total duration), so short requests are visible between the 5 s polls; in-memory, oldest start first — and `last_used` — per-user most recent usage row within 24 h `[{"user","last_used"}]`, newest first. The dashboard renders both as relative times ("1 min ago") and refreshes them with the same 5 s poll.
  `buckets`: one entry per minute over the last `window` minutes, zero-filled; a request lands in the minute it **completed** (usage rows are recorded at completion). `active`: in-flight proxied requests per model, all users — in-memory only, cleared by a restart. `models`: enabled catalog models, one row per physical model row with its `id` (group members share a name by design, so consumers must key rows by `id`, not name); `online` = model enabled ∧ backend enabled ∧ backend reachable (GET `/health`, any HTTP answer counts except 5xx — 503 "not ready" and 502/504 from a fronting proxy both mean down; probed at most once per 60 s per backend); `draining` = backend is being taken down (new sessions refused); `concurrency_limit` = that row's slots (0 = unset); `upstream` = engine-side model id, **admin route only** (wiring). On the personal route the model list is group-aware: each ENABLED group with a servable member appears as one synthetic pool row keyed by the **group's** public name (`pool: true`, an explicit marker so consumers need not re-derive it; id = `-groupID`, a synthetic marker that cannot collide with a real model row id; pool-total `active`, summed `concurrency_limit` (0 = unknown), zeroed backend/`online`/`draining`); disabled groups and plain internal (non-member) models are hidden; the pool row **shadows** any same-name public standalone row (the store resolves the name to the group, so the shadowed row is hidden too — the live view matches the catalog); internal member names fold into the group name in `active` and `util` for every group, disabled ones included (their members stay requestable by name), while public member names stay as requested. `util`: per-model utilization — busy % of each minute (`values` aligns with `buckets`), from each request's wall-clock duration distributed across the minutes it occupied, **including streams still running** (their elapsed time is folded in live from the in-flight tracker, so a long request shows busy while it runs, not retroactively at completion); >100 % = parallel streams; `concurrency_limit` (0 = unset) lets the client scale the axis to `limit × 100 %`.

### Agent API (relay port, `sk-` keys)
- `GET /v1/agent-guide` (**no auth**, `text/markdown`) — the integration guide to hand to an AI agent: token creation, MCP config, REST usage, status semantics. The `PUBLIC_RELAY_URL` server root (no `/v1` by convention) and `PUBLIC_DASHBOARD_URL` are interpolated into the text (unset relay keeps the placeholder visible; unset dashboard drops the mention).
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
- `GET /api/admin/backends` → `{"backends":[{"id":1,"name":"halogen","base_url":"http://llm-host:8731","has_key":false,"enabled":true,"draining":false,"online":true,"health_error":"","last_probe_at":"...|null","last_probe_error":"...|null","server_version":"...|null","created_at":"...","model_count":2}]}` — `online` is the **live** reachability verdict (shared health cache, re-probed at most once per minute per backend; disabled backends are offline without a probe) and `health_error` explains a live `false`; `last_probe_*` are the stored result of the last manual Probe only
- `POST /api/admin/backends` — `{"name","base_url","api_key"?}` → `201` backend object
- `PATCH /api/admin/backends/{id}` — any of `{"name","base_url","api_key","enabled","draining"}` (`api_key:""` clears) → backend object
- `DELETE /api/admin/backends/{id}` → `204` · `409` if catalog models still reference it
- `POST /api/admin/backends/{id}/probe` → `{"ok":true,"server_version":"0.18.22|","error":"","models":[{"id":"halogen-qwen3.8-flash-next","in_catalog":true,"model_id":3|null}]}`
- `POST /api/admin/backends/{id}/models` — `{"model_ids":["halogen-qwen3.8-flash-next"]}` → `201 {"created":[{"id":3,"name":"halogen-qwen3.8-flash-next","upstream_model":"halogen-qwen3.8-flash-next"}]}` (skips already-present ids)

### Admin — models (public catalog)
- `GET /api/admin/models` → `{"models":[{"id":1,"name":"halogen","backend_id":1,"backend_name":"halogen","upstream_model":"halogen-qwen3.8-flash-next","visibility":"public","enabled":true,"price_in":100,"price_out":200,"price_cache_read":10,"price_cache_write":0,"context_length":262144,"concurrency_limit":4,"busy_threshold_pct":80,"bench_tok_per_s":41.2,"bench_pp_tok_per_s":1850.4,"bench_at":"...|","fed_visibility":"off","fed_peer_ids":[]}]}`
- `PATCH /api/admin/models/{id}` — any of `{"name","enabled","visibility","price_in","price_out","price_cache_read","price_cache_write","context_length","concurrency_limit","busy_threshold_pct","bench_tok_per_s","bench_pp_tok_per_s","fed_visibility","fed_peer_ids"}` → model object. `visibility` is `"public"` (listed on `/v1/models`) or `"internal"` (hidden from the catalog but still directly requestable); group members are forced `internal` by the store on every membership edit. `bench_tok_per_s`/`bench_pp_tok_per_s` (float ≥0) set the generation/prompt-processing speed manually and clear `bench_at` (configured value, not a measured benchmark). `busy_threshold_pct` (0–10000, else `400`) is the mean-busy % at which the agent surfaces report `busy`; `0` = default 80, values over 100 require that many parallel streams (300 = busy from 3 concurrent). New models are created with `80`.
- `POST /api/admin/models/{id}/benchmark` — runs two timed **streaming** calls against the upstream. Decode pass (TG): natural-prose prompt; speed = completion tokens over first→last token, so speculative/MTP decoding is measured at realistic acceptance rates. Prefill pass (PP): ~10k-token randomized filler prompt (shrunk to ¾ of `context_length` when that is smaller), `max_tokens=1`; speed = `prompt_tokens` over request→first token (prefill completes before the first token is emitted). May take minutes; `409` while another benchmark runs. → `{"bench_tok_per_s":41.2,"completion_tokens":700,"seconds":17.0,"bench_pp_tok_per_s":1850.4,"prompt_tokens":9876,"pp_seconds":5.3,"bench_at":"..."}`. A failed prefill pass never voids the decode result: TG is stored, the previous PP value is kept, and the response carries `"pp_error":"..."` instead of the `bench_pp_tok_per_s`/`prompt_tokens`/`pp_seconds` fields.
- `DELETE /api/admin/models/{id}` → `204`

### Admin — model groups
A group pools several catalog model rows (typically the same model on different backends) under one public name; the proxy resolves the name to one member per session (sessions stay pinned to their member). The group owns its name: it may not collide with a public model name or another group. Members are forced `internal` while they belong to the group and become `public` again when they leave; a member row stays directly requestable either way.
- `GET /api/admin/groups` → `{"groups":[{"id":1,"name":"qwen3-coder","enabled":true,"member_count":2,"members":[{"model_id":3,"weight":41.2,"position":0,"model_name":"qwen3-coder-a","backend_name":"gpu1","upstream_model":"qwen3-coder","visibility":"internal","model_enabled":true,"backend_enabled":true,"backend_draining":false}]}]}`
- `POST /api/admin/groups` — `{"name"}` → `201` group object (empty `members`). New groups are born **disabled**; enabling requires ≥1 member. `400` blank/invalid name · `409` name taken.
- `GET /api/admin/groups/{id}` → group object (same shape as list entries) · `404` unknown id
- `PATCH /api/admin/groups/{id}` — partial merge of any of `{"name","enabled","members"}` → group object. `members` is a **full replacement** list of `{"model_id","weight","position"}` (≤256 entries, else `400`): `weight` 0/omitted seeds from the model's `bench_tok_per_s` (relative share of new sessions); `position` is the member order. `409` when the name would shadow a public model or another group, when a model already belongs to a different group, when the same model is listed twice in `members`, or when enabling a group with no members.
- `DELETE /api/admin/groups/{id}` → `204` (members are released, not deleted)

### Admin — settings
- `GET /api/admin/settings` → `{"currency":"EUR"}`
- `PUT /api/admin/settings` — `{"currency":"USD"}` → same shape

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
- `GET /api/admin/peers` → all peers, same shape (owner name + `shared_models` = models visible to that peer: every `fed_visibility:"all"` model + its allowlist rows)
- `POST /api/admin/peers/{id}/rotate` → `{"token":"fed-…"}` · `PATCH /api/admin/peers/{id}` — `{"enabled":true|false}` → peer object (disable is the incident lever: it drops the peer's live channel at once, `1008 "disabled"`) · `DELETE /api/admin/peers/{id}` → `204` (live channel dropped too) — all `404` on unknown id

Federation settings (JWT, admin):
- `GET /api/admin/federation` → `{"role":"standalone","hub_url":"","hub_token":""}` — `role` is `standalone` (default) | `hub` | `spoke`; `hub_token` is echoed back (it is the hub's token the admin pasted, not a secret this server minted). Spoke config is plaintext-at-rest like other DB secrets here; the spoke must present it verbatim.
- `PUT /api/admin/federation` — full replacement `{"role","hub_url","hub_token"}` → same shape. `400` when the role is not one of the three, or a spoke lacks an http(s) `hub_url` or the `hub_token`. Leaving spoke mode clears `hub_url`/`hub_token`. Live-safe: the spoke dialer starts/stops with the role, and moving away from `hub` closes every live channel (`1008 "role changed"`) — no restart needed.

Per-model sharing switch (rides the model object everywhere the admin API
returns models): `PATCH /api/admin/models/{id}` gains `fed_visibility`
(`"off"` default | `"all"` | `"allowlist"`, else `400`) and `fed_peer_ids`
(replaces the peer allowlist wholesale; only accepted together with
`fed_visibility:"allowlist"`, unknown peer id → `400`). PATCH semantics:
switching to `"allowlist"` without `fed_peer_ids` keeps the stored list;
switching to `"off"`/`"all"` clears it (those modes share with
nobody/everybody). The model row surfaces both as `fed_visibility` +
`fed_peer_ids`.

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
  read limit 4 MiB/frame. Types: `hello`, `hello_ack`, `ping`, `pong`
  (implemented now) and `catalog`, `status`, `req`, `res`, `res_body`,
  `res_end` (codec defined, handlers land with task 3; unknown types are
  ignored for forward-compat). Binary payloads (`req.body`,
  `res_body.data`) ride base64 inside the JSON.
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


Admin usage = same summary/detail endpoints under `/api/admin/usage/...` with extra optional `user_id`, `key_id` params; detail rows always include `username`.

## Proxy API (API keys) — served on the **relay port** (default `:8080`)

`Authorization: Bearer sk-...` · `401` unknown/revoked key · `404` unknown or disabled model (OpenAI-style error).
Routes: `POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/responses`, `POST /v1/messages`, `POST /v1/messages/count_tokens` (pass-through, not logged), `POST /v1/embeddings`, `GET /v1/models`.
**Drain mode**: while the backend serving the requested model has `draining` set, requests starting a *new* session (session id = `X-Session-Id` header or the derived conversation hash; empty counts as new) are refused with `503` + `Retry-After: 60`; sessions active within the last 15 minutes (in-memory tracker, `usage_log` fallback after a relay restart) pass through — graceful shutdown for running conversations.
`GET /v1/models` → `{"object":"list","data":[{"id":"<public name>","object":"model","owned_by":"llm-relay"}]}`

## Misc
- `GET /healthz` → `200 {"status":"ok"}` (no auth)
- SPA served at `/` (index.html fallback for client-side routes).
