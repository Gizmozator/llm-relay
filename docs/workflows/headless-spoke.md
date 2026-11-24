# Headless spoke — bring-up runbook

A **headless spoke** is for "I have one local model on one machine and want
to add it to a federation": pure env config, no dashboard interaction,
exactly one model (`MODEL_NAME`), restart to change anything. The **full
relay server** is everything beyond that: several models/machines, pricing
per model, users/keys, the dashboard. The headless box is still a full relay
internally — it just never needs the dashboard.

Design + decisions: `docs/plans/2026-11-24-headless-spoke-v1.md`.
Env reference: README **Headless spoke** section + `.env.example`.

## Bring-up

1. **Hub side** (once): on the hub's dashboard, *My keys → Server token* →
   create a `fed-…` token. Note the hub's public relay URL (the URL spokes
   dial, i.e. the relay port, not the admin port).
2. **Spoke side**: set the env (see `.env.example`):

   ```
   JWT_SECRET=…            # still required (the dashboard exists, unused)
   HEADLESS=true
   BACKEND_URL=http://engine.example.fi:8080
   MODEL_NAME=qwen3-coder
   HUB_URL=https://relay.example.fi
   HUB_TOKEN=fed-…
   ```

   Missing/empty `BACKEND_URL`, `MODEL_NAME`, `HUB_URL` or `HUB_TOKEN` =
   the relay refuses to start (`config: X is required when HEADLESS=true`).
3. **Start it.** The log shows the bootstrap:
   `headless: backend "headless" → model "qwen3-coder" → hub https://…`
   and then the ordinary federation dialer lines.
4. Changing anything (model id, prices, hub token) = edit env, restart.
   Every boot re-applies the env over the stored rows.

## Verify on the hub

- **Admin → Federation → Servers**: the peer shows **online** with its
  version and 1 shared model.
- The model appears in the hub's catalog (`GET /v1/models` or the dashboard)
  like any federated model, priced as the spoke's `PRICE_*` env said
  (prices are spoke-owned; the hub just mirrors them).
- Real proof — request it through the hub:

  ```bash
  curl -s $HUB/v1/chat/completions -H "Authorization: Bearer sk-…" \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen3-coder","messages":[{"role":"user","content":"hi"}]}'
  ```

## Common failures

- **Hub unreachable** (`fed: dial …` retries in the log, Servers page stays
  offline): wrong `HUB_URL` (must be the hub's *relay* URL with scheme),
  wrong/expired `HUB_TOKEN` (rotate on the hub and restart the spoke), or a
  firewall between. The spoke keeps retrying with backoff — fix and it joins
  without a spoke restart.
- **Backend not up yet**: boot succeeds anyway; the log says
  `backend not reachable at boot … health probe will track it`. When the
  backend comes up the health probe flips the backend green; no restart
  needed for connectivity (but a restart re-runs the boot check).
- **`MODEL_NAME` not served by the backend**: the boot logs
  `WARNING: backend does not serve MODEL_NAME=…` with the ids it does list
  — check the id (it must match the upstream id exactly) or restart after a
  hot-swap. Requests for a model the backend does not have fail at request
  time until the env is corrected.
- **Boot refused with `model name … already belongs to another backend`**:
  a catalog row on a different backend owns that name. Rename that row in
  the dashboard or change `MODEL_NAME`.
- **Prices look wrong on the hub**: they are the spoke's — fix `PRICE_*` on
  the spoke and restart; the hub mirrors the next catalog push.
