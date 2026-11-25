# Plan: Usage data retention — bounded usage_log with an admin setting

- **Status:** draft
- **Owner:** maintainer
- **Started:** 2026-11-25
- **Issue:** — (operator request; companion to the "keep SQLite" decision)
- **Supersedes / superseded by:** —

## Goal

`usage_log` is the only table that grows without bound. Give the operator a
retention setting on the admin Settings page — delete usage rows older than
N days — so the SQLite file stays bounded without external cron jobs or a
bigger database engine. **Default: keep forever** (no surprise data loss on
upgrade; retention is opt-in).

## Non-goals

- No archiving/export (deleted is deleted; operators who want history keep
  the forever default or back up the file).
- No VACUUM after purge: freed pages are reused by future rows; the file
  does not shrink (VACUUM is the most expensive op SQLite has and a
  write-lock stall — the exact failure mode this project just fixed).
- No per-table retention: only `usage_log` grows meaningfully.
- No timezone-local cutoffs: `ts` is UTC RFC3339 text, cutoffs are UTC.

## Current state

`settings(key,value)` table with `GetSetting`/`SetSetting` (store.go);
`GET/PUT /api/admin/settings` carries `currency` only (admin.go). No
background jobs exist in the relay process today; startup is
`cmd/llm-relay/main.go`. Nothing ever deletes usage rows.

## Approach

- **Setting**: `usage_retention_days` — `"0"` (default, keep forever) or
  `30..3650`. Served by the existing settings pair:
  `GET /api/admin/settings` → `{"currency":"EUR","retention_days":0}`,
  `PUT` accepts `retention_days` (absent = unchanged; invalid → 400).
- **Purge** (`store.PurgeOldUsage(db, cutoff)`): batched delete loop —
  `DELETE FROM usage_log WHERE id IN (SELECT id FROM usage_log WHERE ts < ?
  LIMIT 1000)` repeated until a pass removes 0 rows. Each statement is its
  own short transaction: a single big DELETE would hold the write lock for
  seconds and starve the 4-slot pool (the 504 lesson). Returns the deleted
  row count (logged).
- **Schedule**: a goroutine started at boot — purge once shortly after
  startup, then every 24 h; reads the setting fresh each run, so a setting
  change takes effect at the next daily pass. A settings PUT that lowers
  retention also fires one async purge pass (immediate feedback).
- **UI**: Settings page gains a "Usage data retention" select: Keep
  forever (default) / 30 / 90 / 180 / 365 / 730 days, saved with the
  existing save flow.

## Tasks

- [ ] `store`: `PurgeOldUsage` (batched), setting constant + validation
  helper.
- [ ] `api`: settings pair carries `retention_days`; async purge on
  lowering PUT.
- [ ] `cmd`: daily purge goroutine (startup + 24 h ticker), context-
  cancellable with shutdown.
- [ ] Tests: store (cutoff math, batching >1000 rows, forever = no-op,
  boundary ts == cutoff kept), api (settings round-trip, validation,
  purge-on-put).
- [ ] Docs: README settings paragraph, `docs/api-contract.md` settings
  pair, this plan.

## Verification

- `make ci` green.
- Store test proves rows at exactly the cutoff survive (`ts < cutoff` is
  strict) and a >batch-size backlog drains fully in one call.
- Manual: set 30 days on the test deployment, confirm log line
  `retention: purged N usage rows` and that the Usage page oldest row
  stays inside the window.

## Decisions

- **Default keep forever** (operator's call): upgrading must never delete
  history silently; retention is opt-in per instance.
- **Days, not months**: months vary; the setting is a day count, the UI
  offers friendly presets.
- **Batch size 1000**: small enough that each DELETE is milliseconds even
  on the N5195, large enough that a big backlog drains in seconds.
- **No VACUUM / auto_vacuum change**: file reuse is enough at homelab
  scale; shrinking the file is not worth a write-lock stall.

## Open questions

- Whether the Settings page should show current usage row count / DB size
  next to the control (nice-to-have, decide during UI work).

## Changelog

- 2026-11-25: plan written (queued behind the fairness-ledger ship).
