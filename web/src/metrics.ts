// Shared metric definitions for the traffic charts, so the Dashboard and the
// Usage page cannot drift: one definition per metric feeds both dropdowns,
// the chart series labels and the tick/tooltip formatters.
//
// The accessors read the fields LiveBucket (GET /api/live buckets) and
// UsageGroup (GET /usage/summary groups) share; cost_micro exists only on
// usage rows, which is exactly why Cost is in USAGE_METRICS only.

import { fmtInt, fmtMoney, inNewTokens } from './format';

/** The counters both chart row types carry (LiveBucket and UsageGroup both
 *  satisfy this structurally). cost_micro is usage-only, hence optional. */
export interface MetricRow {
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cache_write_tokens: number;
  /** Money in micro-units; absent on live buckets (the live window is not priced). */
  cost_micro?: number;
}

export type MetricId =
  | 'requests'
  | 'total_tokens'
  | 'in_new'
  | 'cached'
  | 'cache_write'
  | 'out'
  | 'cost';

export interface Metric {
  id: MetricId;
  /** Dropdown option text. */
  label: string;
  /** Chart series label, and the Usage panel-title prefix ("Tokens by day"). */
  chartLabel: string;
  /** 'money' → currency ticks (values are micro-units); 'count' → plain integers. */
  axis: 'money' | 'count';
  /** Value in the axis's unit: micro-units for money, counts otherwise. */
  value: (row: MetricRow) => number;
}

const REQUESTS: Metric = {
  id: 'requests',
  label: 'Requests',
  chartLabel: 'Requests',
  axis: 'count',
  value: (r) => r.requests,
};

const TOTAL_TOKENS: Metric = {
  id: 'total_tokens',
  label: 'Total tokens',
  chartLabel: 'Tokens',
  axis: 'count',
  // The disjoint buckets summed: in new + cached + cache-write + out.
  value: (r) => inNewTokens(r) + r.cached_tokens + r.cache_write_tokens + r.completion_tokens,
};

const IN_NEW: Metric = {
  id: 'in_new',
  label: 'In new tokens',
  chartLabel: 'In new tokens',
  axis: 'count',
  value: (r) => inNewTokens(r),
};

const CACHED: Metric = {
  id: 'cached',
  label: 'Cached tokens',
  chartLabel: 'Cached tokens',
  axis: 'count',
  value: (r) => r.cached_tokens,
};

const CACHE_WRITE: Metric = {
  id: 'cache_write',
  label: 'Cache-write tokens',
  chartLabel: 'Cache-write tokens',
  axis: 'count',
  value: (r) => r.cache_write_tokens,
};

const OUT: Metric = {
  id: 'out',
  label: 'Out tokens',
  chartLabel: 'Out tokens',
  axis: 'count',
  value: (r) => r.completion_tokens,
};

const COST: Metric = {
  id: 'cost',
  label: 'Cost',
  chartLabel: 'Cost',
  axis: 'money',
  // The `?? 0` only satisfies the shared row type: live buckets carry no
  // cost, and no page that renders live buckets lists this metric.
  value: (r) => r.cost_micro ?? 0,
};

/** Dashboard dropdown: the full set minus Cost — live buckets deliberately
 *  exclude cost (owner decision; pricing is per-user and lives on Usage).
 *  Requests first: the default, matching the old two-option chart. */
export const LIVE_METRICS: Metric[] = [
  REQUESTS,
  TOTAL_TOKENS,
  IN_NEW,
  CACHED,
  CACHE_WRITE,
  OUT,
];

/** Usage dropdown: everything, Cost first (the default, matching the old
 *  cost-only chart). */
export const USAGE_METRICS: Metric[] = [
  COST,
  TOTAL_TOKENS,
  IN_NEW,
  CACHED,
  CACHE_WRITE,
  OUT,
  REQUESTS,
];

/** Tick/tooltip formatter for one metric: money for Cost (values are
 *  micro-units), plain integers for the token and request series. */
export function formatMetricValue(m: Metric, v: number, currency: string): string {
  return m.axis === 'money' ? fmtMoney(v, currency) : fmtInt(v);
}
