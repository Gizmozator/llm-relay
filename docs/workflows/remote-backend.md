# Remote backend — bring-up runbook

A **remote backend** is for "I have an engine on another machine — often
behind NAT — and want this relay to route traffic to it": the box dials
**in** over a WebSocket and appears as a normal backend on the Backends
page. No inbound ports on the remote box; works on any relay role
(standalone, hub or spoke). The remote box runs the same `llm-relay`
binary in connector mode (`HEADLESS=true`): it binds nothing, owns no
state, and just pipes `/v1/*` + `/health` between the relay and its local
engine — unless you opt into **edge proxy mode** (`PROXY_LISTEN`), where it
also serves its own LAN's clients through the channel.

Design + decisions: `docs/plans/2026-11-24-remote-backend-v1.md`.
Env reference: README **Remote backends** section + `.env.example`.

## Bring-up

1. **Relay side** (once): on the dashboard, *Backends → + Remote backend*,
   give it a name. The modal shows the `rb-…` connect token **once** — copy
   it now; the server stores only its hash and cannot show it again.
2. **Remote box**: copy `deploy/remote-backend/docker-compose.yml` and
   `deploy/remote-backend/.env.example` (rename the latter to `.env`) onto
   the machine that runs the engine, and fill in `.env`: `RELAY_URL` (the
   relay's public **relay-port** URL, **must be `https://`**), `RELAY_TOKEN`
   (the `rb-…` token) and `ENGINE_URL` (the local engine root). `ENGINE_TOKEN`
   only if the engine needs a key. No `JWT_SECRET`, no data volume, no
   published ports — the connector dials out only. The compose file uses
   `network_mode: host` so `ENGINE_URL=http://localhost:PORT` reaches the
   engine on the host; drop that line and put the engine on a shared docker
   network instead if you prefer.

   **TLS**: the channel is TLS-only. If the relay uses a **self-signed** cert
   (generated on its *Settings → TLS* page), also set `RELAY_PIN` to the
   fingerprint shown there. If the relay sits behind a TLS-terminating proxy
   with a real CA cert, leave `RELAY_PIN` empty — the cert verifies normally.

   Missing/empty `RELAY_URL`, `RELAY_TOKEN` or `ENGINE_URL` = the connector
   refuses to start (`config: X is required when HEADLESS=true`); a
   non-`https` `RELAY_URL` or a malformed `RELAY_PIN` is refused the same way.
3. **Start it.** The connector log shows
   `headless: connected to https://relay…; engine http://localhost:…`.
4. **Back on the relay**: the Backends page shows the row **online** (health
   = the live connection). Use **Sync models** to discover what the engine
   serves — the probe rides the tunnel — then price and enable the models
   as with any backend. **Benchmark** rides it too: the timed streams
   travel over the channel to the engine and back.

Changing anything = edit the env, restart the connector. Rotating the token
(*Edit → Rotate token*) kills the live connection immediately; put the new
token in the env and restart the connector. Disable or delete does the same.

## Verify on the relay

- **Backends page**: the row shows **online** while the connector holds the
  channel; the badge says `remote`.
- **Sync models** lists the engine's model ids (probe over the tunnel).
- Real proof — request one through the relay:

  ```bash
  curl -s $RELAY/v1/chat/completions -H "Authorization: Bearer sk-…" \
    -H 'Content-Type: application/json' \
    -d '{"model":"pub-model","messages":[{"role":"user","content":"hi"}]}'
  ```

## Channel admission (tier 5)

The abuse-protection stack (`internal/ratelimit`, README **Abuse
protection**) guards the connect endpoints per source IP, and live edge
traffic per **channel**: every client request forwarded over a backend's
channel passes the tier-5 bucket (`RATELIMIT_REMOTE_CHANNEL_*`, default
`100`/`200`) keyed on the backend id — per-socket IP is the wrong key
here, since one box multiplexes all its users over one source address.
The tunnel itself stays bounded by `fed.MaxInflight` (16 in-flight
requests per channel); the edge listener fails fast with `503` when the
slots are full rather than queueing (see **Edge proxy mode**).

## Edge proxy mode (`PROXY_LISTEN`)

The connector can also serve the box's LAN directly: with `PROXY_LISTEN`
set (e.g. `8080` or `127.0.0.1:8080`), it binds a local `/v1` listener
and forwards client traffic over the channel to the relay, which runs
each request through its full pipeline — key auth, catalog, groups,
metering, routing. The client's `sk-` key rides verbatim (the `rb-`
token authenticates the channel, never a `/v1` request), so usage rows
land on the real key and user, and the relay's per-key limit applies to
them as to any other traffic.

- **Bind discipline**: a bare port or `:port` binds `127.0.0.1`. Only an
  explicit host (`0.0.0.0:…`, a LAN address) binds elsewhere — and that
  is a loud operator decision: the relay cannot defend a public connector
  box. Keep the listener on the LAN.
- **Fail fast, never queue**: local concurrency cap (64, then `503` +
  `Retry-After`), channel slots full → immediate `503`, body capped at
  the tunnel limit, read/header timeouts against slow-loris.
- **Cancel both ways**: the local client hanging up sends a `Cancel`
  frame and the relay aborts the upstream call; a dead channel ends all
  streams it was serving.
- **Loop guard**: a request that arrived over backend X's channel is
  never routed back to X — a group falls over to its other members, a
  single-model request hosted on X is refused. Ordinary (non-edge)
  traffic still routes to X normally.
- **Metering honesty**: the relay sees the connector box as the direct
  peer and the LAN client in `X-Forwarded-For` (honored only with
  `TRUST_PROXY_HEADERS` on, as everywhere).

Verify: from a client machine, `curl -H "Authorization: Bearer sk-…"
http://<box>:8080/v1/chat/completions -d '{"model":"…","messages":[]}'`
answers exactly as it would from the relay, and the Usage page shows the
row against the client's own key.

## Common failures

- **Relay unreachable** (`headless: relay link down …; redial in …`):
  wrong `RELAY_URL` (must be the relay's *relay-port* URL with scheme),
  or a firewall. The connector keeps retrying with backoff — fix and it
  joins without a restart.
- **401 at connect** (`invalid connect token`): wrong or rotated token —
  rotate on the Backends page and restart the connector with the new one.
- **403 at connect** (`backend disabled`): the backend row is disabled;
  enable it on the Backends page.
- **TLS handshake failure / `bad certificate` in the connector log**: the
  relay uses a self-signed cert and `RELAY_PIN` is missing or wrong — paste
  the fingerprint from the relay's *Settings → TLS* page. A `403 "TLS
  required"` means the relay is not serving TLS where you dial: it needs a
  cert (Settings → TLS) or, behind a TLS-terminating proxy, `TLS_PORT=0` **and**
  `TRUST_PROXY_HEADERS=true` so `X-Forwarded-Proto: https` satisfies the gate.
- **429 at connect** (`too many connect attempts`): the per-source-IP
  machine-endpoint bucket is drained — usually repeated bad-token attempts
  (a wrong or rotated token typed into `.env`) or a flapping connector loop.
  A successful connect resets the bucket; see `RATELIMIT_CONNECT_*` in the
  README and fix the token rather than raising the limit.
- **Online but Sync models fails**: the engine is down on the remote box
  (`ENGINE_URL` wrong or the engine not up) — the connector answers the
  probe from the engine, so a dead engine reads as a probe error, not a
  dead backend.
- **Requests fail with `remote backend not connected`**: the channel is
  down (connector restarting); traffic fails over to other group members
  like any dead backend and recovers when the connector redials.
- **Connector restart flaps the row offline/online briefly**: expected —
  the new connection replaces the old one on the handshake; no action
  needed.
- **Edge listener answers `503` (`Retry-After: 1`)**: fail-fast by design
  — the local concurrency cap (64) or the channel's 16 tunnel slots are
  full (long streams). The client should back off; nothing queued up.
- **Edge listener answers `429`**: the tier-5 per-channel bucket is
  drained for this backend (`RATELIMIT_REMOTE_CHANNEL_*`); other boxes'
  channels have their own buckets.
