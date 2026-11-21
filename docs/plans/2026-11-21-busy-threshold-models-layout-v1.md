# Plan: Per-model busy threshold + Models page two-row layout

- **Status:** active
- **Owner:** antti
- **Started:** 2026-11-21
- **Issue:** —
- **Supersedes / superseded by:** extends 2026-11-21-agent-utilization-api-v1.md

## Goal

Two things, one increment:

1. The admin can tune, per model, when the agent surfaces (`/v1/utilization`
   and MCP `check_utilization`) call it **busy**: a new editable
   **Busy ≥ %** field (mean-busy threshold). Today the threshold is the
   compiled constant 80 %. The existing **Concurrency** limit already covers
   the other half of the rule (`active ≥ limit` → busy) and stays as-is.
2. The admin Models page table (12 columns, growing) is split into **two
   rows per model** so it stops getting wider: identity row + a compact
   metrics row that carries the prices, context, concurrency, speed and the
   new threshold.

## Non-goals

- No change to the live dashboard utilization graph (axis still scales by
  concurrency_limit; the threshold is agent-status only).
- No new field in the `/v1/utilization` payload — `status` already reflects
  the threshold; agents do not need the number itself.
- No global (settings-level) default override — the compiled default stays
  80 %; per-model values override it.

## Current state

- `utilization.go`: `const busyPctThreshold = 80.0`; status rule
  `offline > draining > active ≥ concurrency_limit > mean busy ≥ 80 > free`.
- `models` table (0001 + 0003 meta): `context_length`, `concurrency_limit`,
  `bench_tok_per_s`, `bench_at`, all `0 = unset` convention.
- `store.UpdateModelMeta(db, id, contextLength, concurrencyLimit, benchTokPerS)`
  (nil = leave); `AdminUpdateModel` PATCH mirrors it; `AdminModels.tsx`
  renders one `<tr>` per model with `EditableNumber`/`EditableText`/`Toggle`
  cells and merges PATCH responses into local rows.

## Approach

- **DB**: migration `0007_busy_threshold.sql` →
  `ALTER TABLE models ADD COLUMN busy_threshold_pct INTEGER NOT NULL DEFAULT 0`
  (0 = unset → default 80, same convention as the other meta fields).
- **store**: `Model.BusyThresholdPct int64` + `modelCols`/`scanModel`;
  `UpdateModelMeta` gains `busyThresholdPct *int64`.
- **utilization.go**: per-model threshold
  (`thr := m.BusyThresholdPct; if thr == 0 { thr = 80 }`), constant kept as
  the default. Rule text otherwise unchanged.
- **Admin API**: `AdminUpdateModel` accepts `busy_threshold_pct` (0–100,
  else 400) and passes it through `UpdateModelMeta`.
- **AdminModels page — two-row layout**: each model renders two `<tr>`s —
  row 1: public name, backend, upstream id, enabled, actions (Benchmark,
  Delete); row 2 (class `sub-row`, dimmer): the four prices, context,
  concurrency, speed, **Busy ≥ %**. Editing behavior unchanged (per-cell
  PATCH + local merge). Small CSS addition for `.sub-row` in the stylesheet.
- **Docs**: api-contract (PATCH field + status rule wording), agent-guide
  (threshold is per-model admin-set, default 80), README models blurb if it
  mentions the rule.

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | migration 0007 + store field + UpdateModelMeta | — | ☐ |
| 2 | utilization per-model threshold | 1 | ☐ |
| 3 | AdminUpdateModel busy_threshold_pct (0–100) | 1 | ☐ |
| 4 | AdminModels two-row layout + Busy ≥ % cell | 3 | ☐ |
| 5 | tests (store round-trip, threshold respected incl. 0=default, PATCH validation) | 2,3 | ☐ |
| 6 | docs: api-contract + agent-guide + plan changelog | 2 | ☐ |

## Verification

- `make ci` green; `cd web && npm run build` clean.
- Tests: model with `busy_threshold_pct=30` reads `busy` at ~35 % mean busy
  (was `free` under the old constant); `0` keeps the 80 % default; PATCH
  `busy_threshold_pct=101` → 400; store round-trip.
- Manual: Models page shows two rows per model, Busy ≥ % editable, values
  survive reload.

## Decisions

| # | decision | why | rejected alternative |
|---|---|---|---|
| 1 | Threshold = mean-busy % only; concurrency_limit stays the active-slots rule | owner asked for "the limits for free/busy reporting"; the two existing rules stay orthogonal, one new knob | merging both into one threshold (loses the slots signal) |
| 2 | `0 = unset → default 80` | matches context_length/concurrency_limit convention already on the page | default 100 (surprising) or NOT NULL 80 (can't express "unset") |
| 3 | Two-row split: identity+actions on top, metrics below | keeps every existing editor in place, halves the column count, threshold fits without widening | card grid redesign (bigger change, loses table scanability) |
| 4 | Threshold not exposed in the utilization payload | status already encodes the verdict; fewer moving parts for agents | adding busy_threshold_pct to the response (unused by agents) |

## Changelog

- 2026-11-21 — plan created.
