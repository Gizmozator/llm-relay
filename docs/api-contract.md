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
- `GET /api/models` → `{"models":[{"name":"Qwen3.8-flash-next","price_in":100,"price_out":200,"price_cache_read":10,"price_cache_write":40,"context_length":262144,"concurrency_limit":4,"bench_tok_per_s":41.2,"bench_at":"2026-11-20T12:00:00Z"}]}` — models usable right now (enabled model on an enabled backend); prices are micro-units per 1M tokens; `context_length`/`concurrency_limit` are admin-set (0 = unset); `bench_tok_per_s`/`bench_at` are the last admin benchmark (0/"" = never); `bench_tok_per_s` may also be set manually by an admin, which leaves `bench_at` empty. Internal wiring (backend, upstream id) is not exposed.

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
  → `{"buckets":[{"minute":"2026-11-21T12:34","requests":0,"prompt_tokens":0,"completion_tokens":0,"cached_tokens":0,"cache_write_tokens":0}],"active":[{"model":"halogen","count":1}],"models":[{"name":"halogen","backend":"halogen (iGPU)","online":true,"draining":false,"active":1}],"util":[{"model":"halogen","values":[0,12.5],"concurrency_limit":4}]}`
  The live view is **server-wide for every logged-in user** — its job is answering "is the server free" (owner decision); cost is deliberately not exposed here (per-user cost lives on the Usage pages). The admin variant additionally accepts `user_id=<id>` as a drill-down (ignored on the personal route).
  **Admin route only** (absent from the personal response): `connections` — one row per active proxied request `{"user","key","model","endpoint","session","ip","streaming","started_at","seconds"}` (in-memory tracker, oldest first; `seconds` = elapsed run time) — and `last_used` — per-user most recent usage row within 24 h `[{"user","last_used"}]`, newest first. The dashboard renders both as relative times ("1 min ago") and refreshes them with the same 5 s poll.
  `buckets`: one entry per minute over the last `window` minutes, zero-filled; a request lands in the minute it **completed** (usage rows are recorded at completion). `active`: in-flight proxied requests per model, all users — in-memory only, cleared by a restart. `models`: enabled catalog models; `online` = model enabled ∧ backend enabled ∧ backend reachable (GET `/health`, any HTTP answer counts except 5xx — 503 "not ready" and 502/504 from a fronting proxy both mean down; probed at most once per 60 s per backend); `draining` = backend is being taken down (new sessions refused). `util`: per-model utilization — busy % of each minute (`values` aligns with `buckets`), from each request's wall-clock duration distributed across the minutes it occupied, **including streams still running** (their elapsed time is folded in live from the in-flight tracker, so a long request shows busy while it runs, not retroactively at completion); >100 % = parallel streams; `concurrency_limit` (0 = unset) lets the client scale the axis to `limit × 100 %`.

### Agent API (relay port, `sk-` keys)
- `GET /v1/agent-guide` (**no auth**, `text/markdown`) — the integration guide to hand to an AI agent: token creation, MCP config, REST usage, status semantics. The `PUBLIC_RELAY_URL` server root (no `/v1` by convention) and `PUBLIC_DASHBOARD_URL` are interpolated into the text (unset relay keeps the placeholder visible; unset dashboard drops the mention).
- `GET /v1/utilization` (bearer `sk-` key)
  → `{"window_minutes":5,"models":[{"name":"halogen","online":true,"draining":false,"active":1,"concurrency_limit":4,"utilization_pct":37.5,"status":"free"}]}`
  Server-wide by design (delegation decisions are global truth). `utilization_pct` credits streams still running (folded from their start time, not waiting for the usage row written at completion). `status`: `offline` (backend unreachable) | `draining` (admin is taking the backend down: new sessions get `503` + `Retry-After: 60`, sessions active within 15 min may continue) | `busy` (`active ≥ concurrency_limit` when set, or mean `utilization_pct ≥` the model's `busy_threshold_pct` over the window; 0 = default 80) | `free`.
- `POST /mcp` (bearer `sk-` key) — stateless MCP over streamable HTTP (JSON-RPC 2.0, plain JSON responses; no sessions/SSE). Methods: `initialize` (version negotiation, `serverInfo`, `instructions`), `ping`, `notifications/*` → `202`, `tools/list`, `tools/call`. One tool: `check_utilization` (optional `model` string argument filters the list; a non-string argument → `-32602`, a name matching no enabled model → in-band `isError`), returning the REST payload as `structuredContent` + text. Unknown method → `-32601`, unknown tool → `-32602`, bad JSON → `-32700`, oversize body (>1 MiB) → HTTP 413, tool *execution* failures are in-band (`isError:true`, generic message — internal errors are logged, not leaked). Any notification (absent `id`), malformed or not, gets `202` with no body.

### Admin — users
- `GET /api/admin/users` → `{"users":[{"id":1,"username":"a","display_name":"Ada","role":"admin","disabled":false,"created_at":"...","price_multiplier":1,"pass_changed_at":"...|"}]}` (`pass_changed_at` = last password change; JWTs issued earlier are revoked; `""` = never)
- `POST /api/admin/users` — `{"username","display_name"?,"password","role":"admin"|"user"}` → `201` user object
- `PATCH /api/admin/users/{id}` — any of `{"password","disabled","role","display_name","price_multiplier"}` → user object. `price_multiplier` (float >0, default 1) scales every cost recorded for that user at request time (snapshot semantics: changing it never rewrites existing rows). Admin-only: `GET /api/me` deliberately omits it. Guard: an update that would leave zero enabled admins → `400`; setting a password >72 bytes → `400`.

### Admin — backends
- `GET /api/admin/backends` → `{"backends":[{"id":1,"name":"halogen","base_url":"http://llm-host:8731","has_key":false,"enabled":true,"draining":false,"last_probe_at":"...|null","last_probe_error":"...|null","server_version":"...|null","created_at":"...","model_count":2}]}`
- `POST /api/admin/backends` — `{"name","base_url","api_key"?}` → `201` backend object
- `PATCH /api/admin/backends/{id}` — any of `{"name","base_url","api_key","enabled","draining"}` (`api_key:""` clears) → backend object
- `DELETE /api/admin/backends/{id}` → `204` · `409` if catalog models still reference it
- `POST /api/admin/backends/{id}/probe` → `{"ok":true,"server_version":"0.18.22|","error":"","models":[{"id":"halogen-qwen3.8-flash-next","in_catalog":true,"model_id":3|null}]}`
- `POST /api/admin/backends/{id}/models` — `{"model_ids":["halogen-qwen3.8-flash-next"]}` → `201 {"created":[{"id":3,"name":"halogen-qwen3.8-flash-next","upstream_model":"halogen-qwen3.8-flash-next"}]}` (skips already-present ids)

### Admin — models (public catalog)
- `GET /api/admin/models` → `{"models":[{"id":1,"name":"halogen","backend_id":1,"backend_name":"halogen","upstream_model":"halogen-qwen3.8-flash-next","enabled":true,"price_in":100,"price_out":200,"price_cache_read":10,"price_cache_write":0,"context_length":262144,"concurrency_limit":4,"busy_threshold_pct":80,"bench_tok_per_s":41.2,"bench_at":"...|"}]}`
- `PATCH /api/admin/models/{id}` — any of `{"name","enabled","price_in","price_out","price_cache_read","price_cache_write","context_length","concurrency_limit","busy_threshold_pct","bench_tok_per_s"}` → model object. `bench_tok_per_s` (float ≥0) sets the speed manually and clears `bench_at` (configured value, not a measured benchmark). `busy_threshold_pct` (0–10000, else `400`) is the mean-busy % at which the agent surfaces report `busy`; `0` = default 80, values over 100 require that many parallel streams (300 = busy from 3 concurrent). New models are created with `80`.
- `POST /api/admin/models/{id}/benchmark` — runs one timed **streaming** generation (natural-prose prompt; speed = completion tokens over first→last token, so speculative/MTP decoding is measured at realistic acceptance rates) against the upstream and stores the speed; may take minutes; `409` while another benchmark runs. → `{"bench_tok_per_s":41.2,"completion_tokens":700,"seconds":17.0,"bench_at":"..."}`
- `DELETE /api/admin/models/{id}` → `204`

### Admin — settings
- `GET /api/admin/settings` → `{"currency":"EUR"}`
- `PUT /api/admin/settings` — `{"currency":"USD"}` → same shape

Admin usage = same summary/detail endpoints under `/api/admin/usage/...` with extra optional `user_id`, `key_id` params; detail rows always include `username`.

## Proxy API (API keys) — served on the **relay port** (default `:8080`)

`Authorization: Bearer sk-...` · `401` unknown/revoked key · `404` unknown or disabled model (OpenAI-style error).
Routes: `POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/responses`, `POST /v1/messages`, `POST /v1/messages/count_tokens` (pass-through, not logged), `POST /v1/embeddings`, `GET /v1/models`.
**Drain mode**: while the backend serving the requested model has `draining` set, requests starting a *new* session (session id = `X-Session-Id` header or the derived conversation hash; empty counts as new) are refused with `503` + `Retry-After: 60`; sessions active within the last 15 minutes (in-memory tracker, `usage_log` fallback after a relay restart) pass through — graceful shutdown for running conversations.
`GET /v1/models` → `{"object":"list","data":[{"id":"<public name>","object":"model","owned_by":"llm-relay"}]}`

## Misc
- `GET /healthz` → `200 {"status":"ok"}` (no auth)
- SPA served at `/` (index.html fallback for client-side routes).
