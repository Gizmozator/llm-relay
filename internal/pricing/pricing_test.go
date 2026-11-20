package pricing

import (
	"testing"

	"llm-relay/internal/store"
)

func TestCompute(t *testing.T) {
	m := &store.Model{PriceIn: 100, PriceOut: 400, PriceCacheRead: 10, PriceCacheWrite: 125}

	// billable in = 2M - 500k cached - 500k write = 1M → 1M*100/1e6 = 100
	c := Compute(m, Tokens{Prompt: 2_000_000, Completion: 1_000_000, Cached: 500_000, CacheWrite: 500_000})
	if c.In != 100 {
		t.Errorf("In = %d, want 100", c.In)
	}
	if c.Out != 400 {
		t.Errorf("Out = %d, want 400", c.Out)
	}
	if c.Cache != 5 { // 500_000 * 10 / 1e6
		t.Errorf("Cache = %d, want 5", c.Cache)
	}
	if c.Write != 63 { // 500_000 * 125 / 1e6 = 62.5 → round half up
		t.Errorf("Write = %d, want 63", c.Write)
	}
	if c.Total != c.In+c.Out+c.Cache+c.Write {
		t.Errorf("Total = %d, want sum", c.Total)
	}
}

func TestComputeRounding(t *testing.T) {
	m := &store.Model{PriceIn: 1, PriceOut: 0}
	c := Compute(m, Tokens{Prompt: 500_000}) // 0.5 → rounds to 1
	if c.In != 1 {
		t.Errorf("In = %d, want 1 (round half up)", c.In)
	}
	c = Compute(m, Tokens{Prompt: 499_999}) // 0.499999 → 0
	if c.In != 0 {
		t.Errorf("In = %d, want 0", c.In)
	}
}

func TestComputeNegativeBillable(t *testing.T) {
	// cached > prompt (bad upstream data) must not produce negative cost.
	m := &store.Model{PriceIn: 100}
	c := Compute(m, Tokens{Prompt: 10, Cached: 20})
	if c.In != 0 {
		t.Errorf("In = %d, want 0", c.In)
	}
}
