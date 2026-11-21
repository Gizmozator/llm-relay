# Plan: Model catalog price calculator

- **Status:** done
- **Owner:** maintainer
- **Started:** 2026-11-21
- **Issue:** —
- **Supersedes / superseded by:** —

## Goal

An admin editing a model in the catalog can open a **price calculator** from the
model row, enter how the serving machine consumes electricity — power draw and
speed for token generation (TG) and for prompt processing (PP), plus the
electricity price — and the four price fields (input, output, cache read, cache
write) are computed automatically as cost-recovery prices. The admin can tweak
any computed value before applying, and the prices land in the catalog as if
typed by hand.

## Non-goals

- No backend, API or DB changes — calculator inputs are not stored per model.
- No currency conversion; the calculator uses the instance currency as-is.
- No amortization of hardware purchase, idle time or cooling — electricity only.
- No changes to how costs are billed (the existing pricing engine is untouched).

## Current state

- `web/src/pages/AdminModels.tsx`: inline-edit table; each model's sub-row holds
  the four price fields as `EditableNumber`s (displayed currency/1M, stored as
  integer micro-units per 1M tokens) plus context and benchmark speed.
  `patch(id, ModelPatch)` merges the PATCH response into local rows.
- `web/src/components/ui.tsx` provides `Modal`, `Field`, `Banner`; the
  BackendFormModal in `Backends.tsx` is the form-in-modal pattern to follow.
- `bench_tok_per_s` is already measured per model by the Benchmark button.
- Pricing semantics (`internal/pricing`): all money is integer micro-units,
  prices are micro-units per 1M tokens.

## Approach

- New `PriceCalcModal` in `AdminModels.tsx`, opened by a "Calc prices" button in
  each model row's Actions column.
- Inputs (all plain `input.input` numbers):
  - Generation power (W), Generation speed (tok/s) — speed prefilled from the
    model's `bench_tok_per_s` when it has one.
  - Prompt-processing power (W), Prompt-processing speed (tok/s).
  - Electricity price (`currency`/kWh).
- Derived prices, recomputed live whenever an input changes:
  - `cost_micro_per_M(W, tok/s, €/kWh) = €/kWh × W × 1e6 / (3.6 × tok/s)`
    (J per token = W / tok/s; 1 kWh = 3.6 MJ).
  - `price_out` ← TG pair; `price_in` and `price_cache_write` ← PP pair
    (writing cache is prefilling new content); `price_cache_read` = 0
    (cached tokens skip prefill).
  - Each value rounded **up to 1 cent per 1M** (multiple of 10 000 µ; 0 stays 0).
- The four derived prices render as editable inputs; manual edits stick until
  the next calculator-input change (which recomputes all four).
- "Apply prices" PATCHes the four price fields through the existing `patch()`
  and closes; toast on success/failure like other actions.
- Calculator inputs persist in `localStorage` (`llmrelay.pricecalc`) and
  prefill the next open; per-model speed prefill wins over the stored value.
- Apply is disabled while any of the five inputs is empty or ≤ 0.

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | Plan file | — | ☑ |
| 2 | `PriceCalcModal` + "Calc prices" button in AdminModels | 1 | ☑ |
| 3 | localStorage persistence of calculator inputs | 2 | ☑ |
| 4 | Rebuild `web/dist` (`make web`) and verify in browser against demo data | 2 | ☑ |
| 5 | README admin-section note + plan changelog | 2 | ☑ |
| 6 | `make ci`, reviewer gate, commit | 4, 5 | ☐ |

## Verification

- [x] `make ci` green; `npm run build` (tsc + vite) clean, `web/dist` committed.
- [x] Manual: 500 W / 43 tok/s / 500 W / 1000 tok/s / €0.15 kWh → output
      €0.49/1M (484 496 µ → ceil 490 000), input & cache-write €0.03/1M
      (20 833 µ → ceil 30 000), cache-read 0.
- [x] Manual: apply writes the four fields and the row shows the new prices.
- [x] Manual: manual edit of a preview value survives until a calc input changes.
- [x] Edge: empty or zero speed/power/electricity → Apply disabled; reopen
      prefills from localStorage (verified on a second model; per-model bench
      speed wins over the stored value).

## Decisions

| # | decision | why | alternatives considered |
|---|---|---|---|
| 1 | round up to 1 cent / 1M | clean prices, never under-recovers (owner choice) | exact µ value; selectable step |
| 2 | cache read = 0, cache write = PP cost | cached tokens skip prefill; cache write is prefill (owner choice, manual override available) | read = 10 % of input; both = PP |
| 3 | inputs browser-local, not per-model DB columns | calculator is an admin utility; no migration/API churn for convenience data (owner choice) | new model columns + API fields |
| 4 | live recompute, manual edits kept until next input change | predictable, no override bookkeeping | separate Recalculate button |

## Open questions

None blocking.

## Changelog

- 2026-11-21 — plan created.
- 2026-11-21 — implemented: `PriceCalcModal` in `AdminModels.tsx`, verified in
  browser against the demo deployment (math, apply, manual override,
  localStorage prefill). Status → done.
