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

func TestApplyMultiplier(t *testing.T) {
	c := Costs{In: 100, Out: 200, Cache: 10, Write: 0}
	c.Total = c.In + c.Out + c.Cache + c.Write
	ApplyMultiplier(&c, 2.5)
	if c.In != 250 || c.Out != 500 || c.Cache != 25 || c.Write != 0 || c.Total != 775 {
		t.Errorf("2.5x: %+v, want {250 500 25 0 775}", c)
	}
	d := Costs{In: 1, Out: 0, Cache: 0, Write: 0, Total: 1}
	ApplyMultiplier(&d, 1) // default: untouched
	if d.In != 1 || d.Total != 1 {
		t.Errorf("1x changed the costs: %+v", d)
	}
	e := Costs{In: 1, Total: 1}
	ApplyMultiplier(&e, 0.4) // round-half-up: 0.4 → 0
	if e.In != 0 {
		t.Errorf("0.4x of 1 = %d, want 0", e.In)
	}
}
