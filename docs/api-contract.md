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

### Admin — users
- `GET /api/admin/users` → `{"users":[{"id":1,"username":"a","display_name":"Ada","role":"admin","disabled":false,"created_at":"...","price_multiplier":1,"pass_changed_at":"...|"}]}` (`pass_changed_at` = last password change; JWTs issued earlier are revoked; `""` = never)
- `POST /api/admin/users` — `{"username","display_name"?,"password","role":"admin"|"user"}` → `201` user object
- `PATCH /api/admin/users/{id}` — any of `{"password","disabled","role","display_name","price_multiplier"}` → user object. `price_multiplier` (float >0, default 1) scales every cost recorded for that user at request time (snapshot semantics: changing it never rewrites existing rows). Admin-only: `GET /api/me` deliberately omits it. Guard: an update that would leave zero enabled admins → `400`; setting a password >72 bytes → `400`.

### Admin — backends
- `GET /api/admin/backends` → `{"backends":[{"id":1,"name":"halogen","base_url":"http://engine-host:8731","has_key":false,"enabled":true,"last_probe_at":"...|null","last_probe_error":"...|null","server_version":"...|null","created_at":"...","model_count":2}]}`
- `POST /api/admin/backends` — `{"name","base_url","api_key"?}` → `201` backend object
- `PATCH /api/admin/backends/{id}` — any of `{"name","base_url","api_key","enabled"}` (`api_key:""` clears) → backend object
- `DELETE /api/admin/backends/{id}` → `204` · `409` if catalog models still reference it
- `POST /api/admin/backends/{id}/probe` → `{"ok":true,"server_version":"0.18.22|","error":"","models":[{"id":"halogen-qwen3.8-flash-next","in_catalog":true,"model_id":3|null}]}`
- `POST /api/admin/backends/{id}/models` — `{"model_ids":["halogen-qwen3.8-flash-next"]}` → `201 {"created":[{"id":3,"name":"halogen-qwen3.8-flash-next","upstream_model":"halogen-qwen3.8-flash-next"}]}` (skips already-present ids)

### Admin — models (public catalog)
- `GET /api/admin/models` → `{"models":[{"id":1,"name":"halogen","backend_id":1,"backend_name":"halogen","upstream_model":"halogen-qwen3.8-flash-next","enabled":true,"price_in":100,"price_out":200,"price_cache_read":10,"price_cache_write":0,"context_length":262144,"concurrency_limit":4,"bench_tok_per_s":41.2,"bench_at":"...|"}]}`
- `PATCH /api/admin/models/{id}` — any of `{"name","enabled","price_in","price_out","price_cache_read","price_cache_write","context_length","concurrency_limit","bench_tok_per_s"}` → model object. `bench_tok_per_s` (float ≥0) sets the speed manually and clears `bench_at` (configured value, not a measured benchmark).
- `POST /api/admin/models/{id}/benchmark` — runs one timed fixed-prompt generation against the upstream and stores the speed; may take minutes; `409` while another benchmark runs. → `{"bench_tok_per_s":41.2,"completion_tokens":700,"seconds":17.0,"bench_at":"..."}`
- `DELETE /api/admin/models/{id}` → `204`

### Admin — settings
- `GET /api/admin/settings` → `{"currency":"EUR"}`
- `PUT /api/admin/settings` — `{"currency":"USD"}` → same shape

Admin usage = same summary/detail endpoints under `/api/admin/usage/...` with extra optional `user_id`, `key_id` params; detail rows always include `username`.

## Proxy API (API keys) — served on the **relay port** (default `:8080`)

`Authorization: Bearer sk-...` · `401` unknown/revoked key · `404` unknown or disabled model (OpenAI-style error).
Routes: `POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/responses`, `POST /v1/messages`, `POST /v1/messages/count_tokens` (pass-through, not logged), `POST /v1/embeddings`, `GET /v1/models`.
`GET /v1/models` → `{"object":"list","data":[{"id":"<public name>","object":"model","owned_by":"llm-relay"}]}`

## Misc
- `GET /healthz` → `200 {"status":"ok"}` (no auth)
- SPA served at `/` (index.html fallback for client-side routes).
