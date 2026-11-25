// Overflow behaviour of the cost math (final scan fix round, item 8):
// token counts arrive from upstream (or a federation peer) and must never
// wrap a cost negative — a negative line item is free tokens plus a
// corrupted ledger sum.
package pricing

import (
	"math"
	"testing"

	"llm-relay/internal/store"
)

func TestMulDivOverflowSaturates(t *testing.T) {
	// Honest values keep exact results (round-half-up as before).
	if got := mulDiv(1_500_000, 100); got != 150 {
		t.Errorf("mulDiv(1.5M, 100) = %d, want 150", got)
	}
	if got := mulDiv(1, 500_000); got != 1 { // 0.5 → half-up
		t.Errorf("mulDiv(1, 500000) = %d, want 1", got)
	}
	// Hostile inputs: the old (tokens*price + half) wrapped negative.
	for _, tc := range [][2]int64{
		{math.MaxInt64, 5_000_000},
		{math.MaxInt64, math.MaxInt64},
		{1 << 62, 1 << 62},
	} {
		got := mulDiv(tc[0], tc[1])
		if got < 0 {
			t.Errorf("mulDiv(%d, %d) = %d, want saturated non-negative", tc[0], tc[1], got)
		}
		if got > maxComponent {
			t.Errorf("mulDiv(%d, %d) = %d, want <= maxComponent %d", tc[0], tc[1], got, maxComponent)
		}
	}
}

func TestComputeHostileTokensNeverNegative(t *testing.T) {
	m := &store.Model{PriceIn: 5_000_000, PriceOut: 5_000_000,
		PriceCacheRead: 5_000_000, PriceCacheWrite: 5_000_000}
	tok := Tokens{Prompt: math.MaxInt64, Completion: math.MaxInt64,
		Cached: math.MaxInt64, CacheWrite: math.MaxInt64}
	c := Compute(m, tok)
	if c.In < 0 || c.Out < 0 || c.Cache < 0 || c.Write < 0 || c.Total < 0 {
		t.Errorf("hostile tokens produced a negative cost: %+v", c)
	}
	if c.Total > math.MaxInt64 {
		t.Errorf("total overflowed: %+v", c)
	}
}

// ApplyMultiplier must not undo mulDiv's saturation invariant. A raw
// int64(float64(v)*mult) is implementation-defined once the product leaves
// int64 range (amd64 → MinInt64, a large negative cost), and NaN slips past a
// `mult <= 0 || mult == 1` guard. The admin API allows multipliers up to 1000,
// so a saturated component × a big multiplier is reachable with the same
// hostile token counts mulDiv guards against.
func TestApplyMultiplierNeverNegative(t *testing.T) {
	// Saturated components × the max admin multiplier.
	c := Costs{In: maxComponent, Out: maxComponent, Cache: maxComponent, Write: maxComponent}
	c.Total = c.In + c.Out + c.Cache + c.Write
	ApplyMultiplier(&c, 1000)
	for name, v := range map[string]int64{"In": c.In, "Out": c.Out, "Cache": c.Cache, "Write": c.Write} {
		if v < 0 {
			t.Errorf("mult=1000 produced negative %s: %d", name, v)
		}
		if v > maxComponent {
			t.Errorf("mult=1000 %s=%d exceeds maxComponent %d", name, v, maxComponent)
		}
	}
	if c.Total < 0 || c.Total > 4*maxComponent {
		t.Errorf("mult=1000 total out of range: %d", c.Total)
	}

	// A large-but-in-range component must saturate, not wrap.
	d := Costs{In: maxComponent, Total: maxComponent}
	ApplyMultiplier(&d, 8)
	if d.In != maxComponent {
		t.Errorf("saturated component × 8 = %d, want maxComponent %d", d.In, maxComponent)
	}

	// Non-finite multipliers leave the cost untouched (NaN would otherwise
	// pass the <=0/==1 guard and int64(NaN) is garbage).
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		e := Costs{In: 100, Out: 200, Cache: 300, Write: 400, Total: 1000}
		ApplyMultiplier(&e, bad)
		if e.In != 100 || e.Out != 200 || e.Cache != 300 || e.Write != 400 || e.Total != 1000 {
			t.Errorf("non-finite mult %v mutated cost: %+v", bad, e)
		}
	}
}
