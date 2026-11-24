# Remote backend — bring-up runbook

A **remote backend** is for "I have an engine on another machine — often
behind NAT — and want this relay to route traffic to it": the box dials
**in** over a WebSocket and appears as a normal backend on the Backends
page. No inbound ports on the remote box; works on any relay role
(standalone, hub or spoke). The remote box runs the same `llm-relay`
binary in connector mode (`HEADLESS=true`): it binds nothing, owns no
state, and just pipes `/v1/*` + `/health` between the relay and its local
engine.

Design + decisions: `docs/plans/2026-11-24-remote-backend-v1.md`.
Env reference: README **Remote backends** section + `.env.example`.

## Bring-up

1. **Relay side** (once): on the dashboard, *Backends → + Remote backend*,
   give it a name. The modal shows the `rb-…` connect token **once** — copy
   it now; the server stores only its hash and cannot show it again.
2. **Remote box**: copy `deploy/remote-backend/docker-compose.yml` and
   `deploy/remote-backend/.env.example` (rename the latter to `.env`) onto
   the machine that runs the engine, and fill in `.env`: `RELAY_URL` (the
   relay's public **relay-port** URL, with scheme), `RELAY_TOKEN` (the
   `rb-…` token) and `ENGINE_URL` (the local engine root). `ENGINE_TOKEN`
   only if the engine needs a key. No `JWT_SECRET`, no data volume, no
   published ports — the connector dials out only. The compose file uses
   `network_mode: host` so `ENGINE_URL=http://localhost:PORT` reaches the
   engine on the host; drop that line and put the engine on a shared docker
   network instead if you prefer.

   Missing/empty `RELAY_URL`, `RELAY_TOKEN` or `ENGINE_URL` = the connector
   refuses to start (`config: X is required when HEADLESS=true`).
3. **Start it.** The connector log shows
   `headless: connected to https://relay…; engine http://localhost:…`.
4. **Back on the relay**: the Backends page shows the row **online** (health
   = the live connection). Use **Sync models** to discover what the engine
   serves — the probe rides the tunnel — then price and enable the models
   as with any backend.

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

## Common failures

- **Relay unreachable** (`headless: relay link down …; redial in …`):
  wrong `RELAY_URL` (must be the relay's *relay-port* URL with scheme),
  or a firewall. The connector keeps retrying with backoff — fix and it
  joins without a restart.
- **401 at connect** (`invalid connect token`): wrong or rotated token —
  rotate on the Backends page and restart the connector with the new one.
- **403 at connect** (`backend disabled`): the backend row is disabled;
  enable it on the Backends page.
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
