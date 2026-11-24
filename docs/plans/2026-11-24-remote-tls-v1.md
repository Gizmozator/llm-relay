# Plan: TLS for machine-to-machine channels (federation + remote backends)

- **Status:** active
- **Owner:** maintainer
- **Started:** 2026-11-24
- **Issue:** —
- **Supersedes / superseded by:** extends `2026-11-24-remote-backend-v1.md`
  (supersedes the earlier draft of this plan, which scoped TLS to remote backends only)

## Goal

Every machine-to-machine WebSocket channel — spoke→hub federation connect
and connector→relay remote-backend connect — runs over TLS, always. A relay
without a CA-issued certificate mints its own **self-signed certificate
from the dashboard (Settings page)** and shows its **SHA-256 fingerprint**;
the peer (spoke's Federation page, connector's `.env`) pastes that one-line
pin and verifies the relay against it, so an on-path attacker cannot steal
`fed-`/`rb-` tokens or tunnel traffic.

Equally first-class — and the shape of the current reference deployment:
a relay **behind a TLS-terminating reverse proxy with a real certificate**
(`https://relay.example.fi`, plain HTTP between proxy and server). The peer
dials `https://host` (proxy), verifies normally against the system CA store
— no pin, no generated cert, no TLS port needed — and the server accepts the
connection when `X-Forwarded-Proto: https` arrives with `TRUST_PROXY_HEADERS`
on. That switch therefore becomes **required** on proxied deployments before
the gate lands, or their spokes/connectors get `403`; the workflow doc says
so explicitly.

## Non-goals

- ACME / Let's Encrypt automation — obtaining a real cert stays the
  operator's (or proxy's) job; the pin path covers whatever cert arrives.
- mTLS / client certificates — tokens authenticate the dialer.
- TLS for the dashboard/admin port, or for plain `/v1` API traffic —
  this plan gates only the two machine channels' connect endpoints.

## Current state

- Remote backends (plan 2026-11-24-remote-backend-v1, landed): connector
  dials `RELAY_URL` (`http://` or `https://`) with default TLS settings —
  cleartext works, self-signed `https://` fails with no pin option.
- Federation: spoke dials `hub_url` (validated http(s), no scheme
  requirement) via `internal/fed/dialer.go`; hub accepts
  `/federation/v1/connect` + `/federation/v1/ping` (both carry the `fed-`
  token) over whatever the relay port speaks. Spoke config lives in the
  settings table (`hub_url`, `hub_token`), edited on the Federation page.
- `cmd/llm-relay/main.go` binds relay + admin listeners; both connect
  endpoints ride the relay mux. `api.Server` has no proxy-header notion.
- No TLS anywhere in the Go server; deployments put a reverse proxy in
  front (README). `scripts/` federation e2e runs two plain-HTTP relays.

## Approach

**Cert store (migration 0016):** single-row table `remote_tls(id CHECK(id=1),
cert_pem, key_pem, fingerprint, sans, not_after, created_at)`. The private
key is plaintext-at-rest in SQLite — same posture as `hub_token` (it cannot
be hashed; the DB file is the secret boundary). Fingerprint = SHA-256 of the
DER cert, lowercase hex, displayed as `sha256:<hex>`.

**New package `internal/tlscert`:** `Generate(hosts []string)` → ECDSA P-256
self-signed leaf, +3650 days, SANs from input (≥1 DNS name or IP, ≤10),
`server_auth` EKU; `Fingerprint(der)`; `ParsePin(s)` (accepts
`sha256:hex`, bare hex, colons); `DialConfig(pin)` → `tls.Config` that
verifies normally when pin is empty, else `InsecureSkipVerify` +
`VerifyPeerCertificate` requiring a chain cert whose SHA-256 equals the
pin. Used by both dialers and the TLS listener.

**TLS listener:** env `TLS_PORT` (default `8443`, `0` = off). When on, main
serves the **same relay mux** over TLS on that port with
`tls.Config.GetCertificate` reading the current row from the DB — generate,
rotate or delete apply without restart (no row → handshake fails). Plain
relay port keeps serving everything as today.

**Server-side gate (both channels):** `/remote/v1/connect`,
`/federation/v1/connect` and `/federation/v1/ping` refuse with
`403 "TLS required"` unless the request arrived over TLS: `r.TLS != nil`,
or `TRUST_PROXY_HEADERS` is on and `X-Forwarded-Proto: https` (the proxied
deployment). No opt-out — hard rule. `api.Server` gains `TrustProxy bool`.

**Spoke side (hard):** `PUT /api/admin/federation` rejects a non-`https`
`hub_url` for role=spoke; the dialer refuses to dial a non-https URL it
still finds in settings (loud log, no silent cleartext). New optional
setting `hub_pin` (fingerprint) — Federation page gains a field; the dialer
dials with `tlscert.DialConfig(hub_pin)`. Existing stored `http://`
hub_urls stop working at upgrade — intended, see Decisions.

**Connector side (hard):** `config.Load` rejects a non-`https` `RELAY_URL`
when `HEADLESS=true`. New optional `RELAY_PIN` env → same `DialConfig`.
No pin + CA-valid cert (proxy case) verifies normally.

**Admin API:** `GET /api/admin/tls` (sans, fingerprint, not_after,
created_at — never the key), `POST /api/admin/tls` `{"hosts":[...]}` →
generate + view + `cert_pem` (for optional install in an external proxy),
`DELETE` → remove the row. Fed settings gain `hub_pin` (GET/PUT echo it;
public info, stored plaintext like hub_url).

**Dashboard:** **Settings page** gains a “TLS certificate” panel: status
(fingerprint + copy, SANs, expiry) or a generate form (one host per line);
regenerate + delete with confirm; a hint that a TLS-terminating proxy with
a real cert needs none of this (just `TRUST_PROXY_HEADERS`). Federation page
gains the **Hub pin** field; the hub's SetupGuide shows the hub fingerprint
to paste when a self-signed cert is configured. Backends page unchanged
(the token modal gains a `RELAY_PIN=…` hint line when a cert exists).

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | migration 0016 + store/remote_tls.go (single-row CRUD) | — | ☐ |
| 2 | `internal/tlscert`: generate, fingerprint, pin parse, DialConfig | — | ☐ |
| 3 | api: admin GET/POST/DELETE tls + `TrustProxy` + gate on the three endpoints | 1,2 | ☐ |
| 4 | fed settings: `hub_pin` key + https-only hub_url validation + dialer pin/TLS | 2 | ☐ |
| 5 | main.go: TLS listener (GetCertificate from DB) + wiring | 3 | ☐ |
| 6 | config: `TLS_PORT`, https-only `RELAY_URL`, `RELAY_PIN` parse | — | ☐ |
| 7 | connector: dial with `DialConfig(RELAY_PIN)` | 2,6 | ☐ |
| 8 | dashboard: Settings TLS panel; Federation hub-pin field; SetupGuide fingerprint; token-modal hint | 3,4 | ☐ |
| 9 | tests: store, tlscert, api gate (cleartext 403 / TLS ok / XFP±TrustProxy) on both channels, e2e pin connect + wrong-pin refuse (federation + remote), config refusals | 5,7 | ☐ |
| 10 | scripts: federation e2e relays move to TLS (self-signed + pin) | 5,7 | ☐ |
| 11 | docs: README, remote-backend workflow, api-contract, .env.example, deploy env | 9 | ☐ |

## Verification

- [ ] `make ci` green
- [ ] e2e: TLS server with generated cert + dialer with correct pin →
      connects; wrong pin → refused; no pin + self-signed → refused;
      no pin + CA-valid (httptest) cert → connects. Both channels.
- [ ] gate: cleartext connect (both channels) → 403; TLS → passes; plain
      port + `X-Forwarded-Proto: https` + TrustProxyHeaders → passes;
      same without TrustProxyHeaders → 403.
- [ ] config: `RELAY_URL=http://…` (headless) → boot refused; malformed
      `RELAY_PIN` → boot refused; spoke `hub_url=http://…` → `400`.
- [ ] regenerate cert while connected: live channels survive, new
      handshakes need the new pin.
- [ ] manual: generate on dev relay → paste fingerprint into connector env
      and spoke Federation page → both online over `https://host:8443`;
      behind a TLS proxy with real cert → both online with no pin.
- [ ] proxied deployment (the reference shape): TLS proxy with a real cert,
      plain proxy→server hop, `TRUST_PROXY_HEADERS=true` → spoke +
      connector connect with no pin and no generated cert; with the switch
      off → both get `403 TLS required`.

## Decisions

| # | decision | why | alternatives considered |
|---|---|---|---|
| 1 | One TLS story for federation AND remote backends | same threat (stolen `fed-`/`rb-` tokens, tunneled traffic), one mechanism, one UI | rb-only scope (first draft of this plan — user widened it) |
| 2 | Dedicated TLS port serving the relay mux | additive — plain `/v1` users and the admin port untouched; one mux covers both channels | TLS on the relay port (breaks plain users); proxy-only (pushes cert work onto every operator) |
| 3 | SHA-256 fingerprint pin, no CA-PEM option | one pasteable line is the whole UX; the proxy-with-real-cert case needs no pin at all | CA PEM in env (multi-line paste); both (more surface, same job) |
| 4 | Hard refusal everywhere, no opt-out — including existing http spokes | a silent cleartext fallback is how tokens leak; half-enforcement is enforcement nobody relies on | grandfathering stored http hub_urls (a rule with an exception path rots); `RELAY_INSECURE` escape hatch |
| 5 | Key plaintext in the DB row | cannot be hashed; same boundary as `hub_token` | key on disk (second secret location) |
| 6 | `GetCertificate` reads the DB per handshake | rotate/delete without restart | restart-to-apply; fs watch |
| 7 | Gate trusts `X-Forwarded-Proto` only with `TRUST_PROXY_HEADERS` | behind a TLS-terminating proxy `r.TLS` is nil at origin; the existing trust switch already governs spoofable headers — this is the user's stated proxy case | `r.TLS` only (breaks proxied deployments) |
| 8 | Cert UI on Settings, not Backends | the cert serves federation too — it is instance identity, not a backend detail | Backends page (first draft) |
| 9 | Gate covers `/federation/v1/ping` too | ping carries the `fed-` bearer token | leaving ping cleartext-capable (token leak) |

## Open questions

- Warn in the UI as `not_after` approaches? Deferred — 10-year cert, the
  view shows the date.

## Changelog

- 2026-11-24 — plan created (TLS for remote backends: pinned self-signed,
  hard TLS requirement).
- 2026-11-24 — widened per owner: federation spoke→hub covered by the same
  gate + pin; cert UI moved to Settings; reverse-proxy-with-real-cert
  (plain proxy→server hop) made a first-class flow via `X-Forwarded-Proto`
  + `TRUST_PROXY_HEADERS`; spoke https-only becomes a hard rule.
- 2026-11-24 — owner note: the only live deployment is exactly the proxied
  shape (TLS-terminating reverse proxy, real cert, plain origin hop) — the
  no-pin proxy path is the primary case, and `TRUST_PROXY_HEADERS=true` is
  a deployment prerequisite for the gate there.
