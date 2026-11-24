# Plan: Headless spoke

- **Status:** done
- **Owner:** maintainer
- **Started:** 2026-11-24
- **Issue:** #11
- **Supersedes / superseded by:** —

## Goal

A relay can boot fully configured by env — a **headless spoke**: "I have one
local model on one machine and want to add it to a federation." Pure env
config, no dashboard interaction, exactly one model (`MODEL_NAME`), restart
to change anything. Everything beyond that — several models or machines,
per-model pricing, users/keys, the dashboard — is the **full relay server**.
The headless box joins the federation and shares its one model with the hub;
the dashboard stays served and usable, it is simply not needed.

## Non-goals

- Multiple backends or multiple models in headless mode (that is the full
  server's territory).
- A discovery or periodic-sync loop against the backend's `/v1/models`: the
  single row is fully determined by env; restart to change it.
- Hub-side price overrides: **prices stay spoke-owned** (they ride the
  catalog frame from the spoke's row). Hub-side per-peer pricing is
  explicitly out of scope.
- Any new federation protocol surface — the existing dialer, catalog push
  and materializer are reused untouched.

## Current state

- Federation settings live in the `settings` table
  (`store.PutFederationSettings` validates role/hub_url/hub_token);
  `fed.Manager.Reload()` reads them at startup and starts the spoke dialer,
  which pushes the catalog (all `fed_visibility='all'` models) after
  connecting.
- Backends/models are admin-API CRUD in `internal/store`; `backends.name`
  is UNIQUE, `models` carries UNIQUE(backend_id, upstream_model) and
  UNIQUE(backend_id, name).
- `config.Load` is envconfig-style with fail-fast checks (JWT_SECRET).

## Approach

- `internal/config`: new `HEADLESS` block — `HEADLESS`, `BACKEND_URL`,
  `BACKEND_TOKEN`, `MODEL_NAME`, `HUB_URL`, `HUB_TOKEN`, `PRICE_IN`,
  `PRICE_OUT`, `PRICE_CACHE_READ`, `PRICE_CACHE_WRITE`. With `HEADLESS=true`,
  `BACKEND_URL`/`MODEL_NAME`/`HUB_URL`/`HUB_TOKEN` are required (empty =
  `config.Load` error, JWT_SECRET style). With `HEADLESS=false` (default)
  the whole block is ignored — zero behavior change.
- New package `internal/headless`: `Bootstrap(db, cfg, logf)` runs at startup
  after `db.Migrate` and **before** `fedMgr.Reload()`, in order:
  1. upsert backend named `headless` (fixed constant; UNIQUE name makes it
     idempotent) with base_url/api_key/enabled from env;
  2. upsert the single model row keyed by UNIQUE(backend_id, upstream_model):
     `upstream_model=name=MODEL_NAME`, enabled, `fed_visibility='all'`,
     prices from env — via the existing `store.CreateModel` +
     `store.UpdateModelPatch` path;
  3. persist spoke federation settings via `store.PutFederationSettings`
     (its validation is reused, not duplicated).
  4. best-effort boot check: if the backend answers `/v1/models` and does not
     list `MODEL_NAME`, log a warning (typo / backend not up yet). Non-fatal.
- A name collision with a row on **another** backend refuses the boot with a
  clear error (single-row design: no point continuing with an ambiguous
  catalog).
- After bootstrap, `main.go` calls `fedMgr.CatalogChanged()` (the same hook
  the admin API uses) and `Reload()` starts the dialer; the post-connect
  catalog push carries the model to the hub.
- Env is the source of truth: every boot re-applies backend URL/key, model
  fields and prices over previous values (dashboard edits survive only until
  the next restart — that is the headless contract).

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | config env surface + validation + tests | — | ☑ |
| 2 | store helpers (`BackendByName`, `ModelNameTakenOnOtherBackend`) | — | ☑ |
| 3 | `internal/headless` bootstrap + main.go wiring + tests | 1,2 | ☑ |
| 4 | federation integration test (hub materializes the model) | 3 | ☑ |
| 5 | docs: README, .env.example, runbook | 3 | ☑ |

## Verification

- [x] `make ci` green (fmt-check + vet + `go test -race`)
- [x] bootstrap creates backend/model/settings rows with the exact env
  fields (`fed_visibility=all`, prices); second run creates nothing
  (idempotency); env change re-applies on restart
- [x] name collision with another backend → boot error, foreign row untouched
- [x] backend down at boot → boot succeeds + log; backend answering without
  MODEL_NAME → warning, boot succeeds
- [x] integration: headless-spoke model materializes on the hub with env
  prices and is listed + served through the tunnel
- [x] mutation proofs: required-var validation and collision gate (report)

## Decisions

| # | decision | why | alternatives considered |
|---|---|---|---|
| 1 | exactly one model via `MODEL_NAME`; no discovery/sync loop | owner amendment: a headless box serves one model; env fully determines the row, restart to change | `/v1/models` discovery + periodic sync (original brief) — more moving parts, nothing to sync toward |
| 2 | prices stay spoke-owned; hub-side overrides out of scope | prices already ride the catalog frame; hub-side per-peer pricing is a different feature | hub-side price override table — explicitly deferred |
| 3 | collision with another backend's model name = fatal boot error | single-row design has no fallback; an ambiguous catalog is worse than a refused boot | skip+log (fit the multi-row brief, not this one) |
| 4 | bootstrap writes go through existing store functions | validation (hub_url shape, group-name guard, one-tx semantics) is reused, not duplicated | raw SQL in the headless package |
| 5 | bootstrap runs before `fedMgr.Reload()`, then `CatalogChanged()` | settings must exist when the manager reads them; the hook mirrors the admin API so a late hub reconnect re-pushes | starting the manager first (would dial with no settings) |
| 6 | plan file named `2026-11-24-headless-spoke-v1.md` | repo convention (`YYYY-MM-DD-<slug>-vN.md`, feature-dev-loop.md) | the ticket's suggested `headless-spoke-v1-2026-11-24.md` |

## Open questions

None open. (Hub-side price overrides: out of scope, Decision 2.)

## Changelog

- 2026-11-24 — plan created; implemented same day, including the owner's
  mid-flight amendment to the single-model design (discovery + periodic sync
  removed from scope before any of it was written).
