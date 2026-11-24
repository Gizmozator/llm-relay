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
