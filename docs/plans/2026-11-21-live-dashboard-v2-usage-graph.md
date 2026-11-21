# Plan: Per-model utilization % graph (live dashboard)

- **Status:** done
- **Owner:** antti
- **Started:** 2026-11-21
- **Issue:** —
- **Supersedes / superseded by:** extends 2026-11-21-live-dashboard-v1.md

## Goal

The Dashboard gains a second graph: **utilization % per model** over the last
hour. Each minute shows how busy the model was — the summed wall-clock time
of requests against that model, as a share of the minute. 100 % = one stream
busy the whole minute; more than 100 % = parallel streams (the engine runs
multiple slots, so this is meaningful, not an error).

## Non-goals

- No engine-reported KV-cache/slot metrics (relay-observed busy time only).
- No change to the existing requests/tokens/cost chart.
- No attribution of still-running streams to the current minute before they
  complete (the in-flight badge already covers "right now").

## Current state

`usage_log` stores `latency_ms` and `model_requested` per row; rows land at
completion. `LiveBuckets` (store) already defines the minute window;
`/api/live` already scopes by user. `LineChart` renders a single dataset.

## Approach

- **store.BusyByModel** — fetch `(ts, latency_ms, model_requested)` for the
  window (a row can only overlap the window if it completed inside it),
  distribute `[ts − latency, ts]` across the minute series with clipping at
  both ends, aggregate busy-ms per (model, minute). Values align with the
  `LiveBuckets` window.
- **/api/live** gains `util: [{model, values:[% per minute], concurrency_limit}]`
  (busy_ms / 60000 × 100), same scoping as buckets/active.
- **LineChart** gains an optional multi-series mode (`series` + `legend` +
  `yMax` props, palette colors); existing single-series callers unchanged.
- **Dashboard** adds a "Utilization by model" panel below the traffic chart;
  y-axis max = `concurrency_limit × 100 %` (4 slots → 400 %), auto when the
  limit is unset.

## Tasks

| # | task | blocked by | done |
|---|---|---|---|
| 1 | store: BusyByModel (overlap math) | — | ☑ |
| 2 | api: util in /api/live | 1 | ☑ |
| 3 | web: multi-series LineChart + panel | 2 | ☑ |
| 4 | tests: overlap clipping, split, scoping | 1 | ☑ |
| 5 | docs: api-contract + plan changelog | 3 | ☑ |

## Verification

- [ ] `make ci` green
- [ ] fixed-clock test: a request spanning two minutes splits its busy time
      at the minute boundary; a request starting before the window clips
- [ ] live check: two parallel streams show ~200 % in the completion minutes

## Decisions

| # | decision | why | alternatives considered |
|---|---|---|---|
| 1 | overlap distribution, not completion-minute sum | a 5-min stream is a plateau, not a 500 % spike at the end | sum-at-completion (spiky, >100 % noise) |
| 2 | allow >100 %; y-axis max = model `concurrency_limit` × 100 % (owner: 4 slots → scale to 400 %) | parallel streams are real utilization; the axis shows how full the engine's slots are | cap at 100 % (hides concurrency); fixed axis (wastes space for limit-1 models) |
| 3 | compute in Go, not SQL | minute-overlap math in SQLite needs a series CTE; Go is clearer and testable with a fixed clock | SQL recursive CTE |

## Open questions

- (none)

## Changelog

- 2026-11-21 — plan created.
- 2026-11-21 — implemented: `store.BusyByModel` (Go-side overlap distribution,
  fixed-clock tested incl. window clipping and user scoping), `util` in
  `/api/live` (pct per minute + `concurrency_limit`), multi-series
  `LineChart` (`series`/`legend`/`yMax` props), Dashboard "Utilization by
  model" panel with axis max = limit × 100 % (4 slots → 400 %).
