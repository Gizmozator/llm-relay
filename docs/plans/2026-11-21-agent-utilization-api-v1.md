# Plan: Agent-facing utilization API + guide

- **Status:** done
- **Owner:** antti
- **Started:** 2026-11-21
- **Issue:** —
- **Supersedes / superseded by:** extends 2026-11-21-live-dashboard-v2-usage-graph.md

## Goal

A friend's AI agent can check whether a model on this relay is free before
delegating a subagent task: it fetches a plain-markdown guide that explains
everything (including how its human creates an access token), then calls a
small JSON endpoint with its existing `sk-` key and gets per-model
`online / active / concurrency_limit / utilization_pct / status`.

## Non-goals

- No new token type or scopes — relay `sk-` keys are the access tokens.
- No reservation/queueing API (check-only; races are the caller's problem).
- No auth on the guide (it contains no secrets; it must be readable pre-token).
- No per-user scoping on the endpoint — "is the server free" is global truth.

## Current state

- Relay port (`:8080`) serves `/v1/*` with `sk-` bearer auth
  (`Handler.authKey`); dashboard port has the live view (`/api/live`).
- Pieces exist but live on the dashboard side: `proxy.InFlight` (global
  counts via `Snapshot(0)`), `store.BusyByModel` (busy ms per model/minute),
  `proxy.Reachable` + `api.healthCache` (60 s TTL).
- `PUBLIC_RELAY_URL` env → `api.Server.RelayURL` (guide needs it on the
  relay side too).

## Approach

- **Move `healthCache`** from `internal/api/live.go` to `internal/proxy`
  (next to `Reachable`) as `proxy.HealthCache`; both servers get one
  instance each. Same 60 s TTL, same WithoutCancel probe rule.
- **Shared core** — `Handler.Utilization(ctx)`: per enabled model →
  `online` (HealthCache), `active` (`Flight.Snapshot(0)`),
  `concurrency_limit`, `utilization_pct` (mean busy % over the last 5 min
  via `BusyByModel`, unscoped), and derived `status`: `offline` if backend
  unreachable; `busy` if `active ≥ concurrency_limit` (when set) or
  `utilization_pct ≥ 80`; else `free`.
- **`GET /v1/utilization`** (relay port, `sk-` auth) →
  `{"window_minutes":5,"models":[...]}` — the universal surface.
- **`POST /mcp`** (relay port, `sk-` auth) — hand-rolled **stateless** MCP
  (JSON-RPC 2.0, plain JSON responses, no sessions/SSE): `initialize`
  (version negotiation), `ping`, `tools/list` (one tool
  `check_utilization`), `tools/call` (returns the same payload as
  `structuredContent` + text). Unknown method → `-32601`. No new deps.
- **`GET /v1/agent-guide`** (relay port, **no auth**, `text/markdown`) —
  embedded template with the public relay URL interpolated: what the server
  is, how the human creates a key (dashboard → My keys), the MCP config
  snippet, the REST curl, response schema, suggested decision logic
  (free → go; busy → wait/retry; offline → skip).
- **Dashboard**: dedicated **Agent API** page (nav item, all roles) — the
  copyable "give this link to your AI" guide URL, token pointer to My keys,
  MCP config snippet, REST curl and the status legend.
- main.go: register the three routes on the relay mux; `proxy.Handler`
  gains `RelayURL` + `Health`.

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | proxy: HealthCache moved from api | — | ☑ |
| 2 | proxy: Utilization core + REST handler | 1 | ☑ |
| 3 | proxy: stateless MCP adapter (POST /mcp) | 2 | ☑ |
| 4 | proxy: /v1/agent-guide (embedded md) | — | ☑ |
| 5 | main.go wiring (routes + RelayURL + Health) | 2,3,4 | ☑ |
| 6 | web: Agent API guide page (nav + route) | 4 | ☑ |
| 7 | tests: auth, shape, status logic, MCP methods, guide public | 5 | ☑ |
| 8 | docs: api-contract + README + changelog | 5 | ☑ |

## Verification

- [ ] `make ci` green
- [ ] `curl -s :8080/v1/agent-guide` → 200 markdown without a key
- [ ] `curl :8080/v1/utilization` without key → 401; with key → JSON;
      during a live generation `active`/`utilization_pct` rise, `status`
      flips to busy at the limit
- [ ] disabled backend → model `offline` within ~60 s
- [ ] guide shows the configured PUBLIC_RELAY_URL, not localhost

## Decisions

| # | decision | why | alternatives considered |
|---|---|---|---|
| 1 | reuse `sk-` keys as agent tokens | agents already have them; zero new auth surface | JWT login for agents (humans-only flow), new token type |
| 2 | guide public, data endpoints keyed | the guide's job is to explain token acquisition — must be reachable pre-token | key-gated guide (chicken-and-egg) |
| 3 | derived `status` field with documented thresholds | delegating agents shouldn't invent thresholds; raw numbers still returned | raw numbers only |
| 4 | 5-min utilization window | matches "is it free now" without being twitchy | 1 min (noisy), 15 min (stale) |
| 5 | hand-rolled stateless MCP, no SDK | one tool; the stateless JSON-RPC subset is ~150 lines and matches the repo's stdlib-first style (SSE is already hand-rolled); SDK is session-oriented and heavier | `modelcontextprotocol/go-sdk` (spec-complete, revisit if MCP grows more tools) |
| 6 | live dashboard view is **server-wide for every role**, and cost is removed from it entirely | owner: the whole point is any user can see the server is saturated (one user at 400 % must be visible to the next); cost is per-user privacy and belongs on the Usage pages | v1 per-user scoping of the live view (superseded by owner directive) |
| 7 | dashboard gets a dedicated **Agent API page** (not a Keys-page panel) | owner: "add a page … give this link to your AI" — a nav-visible guide page with the copyable public link | small panel on the Keys page (easy to miss) |

## Open questions

- (none)

## Changelog

- 2026-11-21 — plan created.
- 2026-11-21 — implemented: `proxy.HealthCache` (moved from api, same rules);
  `Handler.Utilization` core (5-min mean busy %, derived
  free/busy/offline status); `GET /v1/utilization` + stateless
  `POST /mcp` (`check_utilization` tool) on the relay port with `sk-` auth;
  public `GET /v1/agent-guide` (embedded markdown, relay URL interpolated);
  dashboard **Agent API** page with the copyable guide link.
- 2026-11-21 — fix: guide/MCP URLs doubled `/v1` (`/v1/v1/agent-guide`)
  because `PUBLIC_RELAY_URL` was set as the OpenAI-style base ending in
  `/v1`. Convention changed instead of patched: **`PUBLIC_RELAY_URL` is the
  server root** (no `/v1`); the embedded guide and Agent API page link
  `{root}/v1/agent-guide`, `{root}/v1/utilization`, `{root}/mcp` directly,
  and the Keys/Settings pages append `/v1` when showing the OpenAI base.
  Set `PUBLIC_RELAY_URL=https://llm.rantaeskola.fi` in the stack env.
- 2026-11-21 — owner directives folded in: live dashboard is server-wide for
  every role (personal `/api/live` no longer scopes to the caller) and cost
  is removed from the live view end-to-end (`LiveBucket.CostMicro`,
  `currency` gone from the response; traffic chart keeps requests/tokens
  only). `make ci` green incl. new agent-endpoint tests.
