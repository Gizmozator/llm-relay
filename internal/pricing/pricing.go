// Package pricing computes request cost snapshots from per-model prices.
// All money is integer micro-units; prices are micro-units per 1M tokens.
package pricing

import (
	"math"

	"llm-relay/internal/store"
)

// Tokens are the normalized token counts of one request:
// Prompt is the TOTAL input (cached/cache-write included), matching the
// OpenAI convention; extractors normalize other dialects to it.
type Tokens struct {
	Prompt     int64
	Completion int64
	Cached     int64
	CacheWrite int64
}

// Costs is the money snapshot stored with the usage row.
type Costs struct {
	In    int64
	Out   int64
	Cache int64
	Write int64
	Total int64
}

const million = 1_000_000

// maxComponent bounds one cost line item: four of them still sum into
// Costs.Total without overflowing int64. Reaching it means the input was
// absurd (hostile or corrupt token counts), not a billable request.
const maxComponent = math.MaxInt64 / 4

// mulDiv computes tokens*price/million with round-half-up. A hostile or
// corrupt token count (upstream-reported, peer-forwarded) near int64 max
// would wrap tokens*price into a NEGATIVE cost — free tokens and a
// corrupted ledger sum — so the multiply is overflow-guarded in the
// quotient domain and saturates at maxComponent instead.
func mulDiv(tokens, price int64) int64 {
	if tokens <= 0 || price <= 0 {
		return 0
	}
	whole, rem := tokens/million, tokens%million
	if price > maxComponent/(whole+1) {
		return maxComponent
	}
	cost := whole * price
	if rem > 0 {
		// rem < million, but price is admin-set and unbounded: guard the
		// second product the same way (the +million/2 rounding term fits
		// under the bound by construction).
		if price > (maxComponent-cost)/(rem+1) {
			return maxComponent
		}
		cost += (rem*price + million/2) / million
	}
	if cost > maxComponent {
		return maxComponent
	}
	return cost
}

// Compute returns the cost of tok under model m's prices.
func Compute(m *store.Model, tok Tokens) Costs {
	// Clamp the cache buckets into [0, Prompt] before billing: a hostile or
	// buggy upstream must not inflate the cache line items past the total.
	prompt := tok.Prompt
	if prompt < 0 {
		prompt = 0
	}
	cached, cacheWrite := tok.Cached, tok.CacheWrite
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	if cacheWrite > prompt-cached {
		cacheWrite = prompt - cached
	}
	billableIn := prompt - cached - cacheWrite
	c := Costs{
		In:    mulDiv(billableIn, m.PriceIn),
		Out:   mulDiv(tok.Completion, m.PriceOut),
		Cache: mulDiv(cached, m.PriceCacheRead),
		Write: mulDiv(cacheWrite, m.PriceCacheWrite),
	}
	c.Total = c.In + c.Out + c.Cache + c.Write
	return c
}

// ApplyMultiplier scales a cost snapshot by a user's price multiplier
// (round-half-up per component). Multiplier 1 — the default — changes nothing.
func ApplyMultiplier(c *Costs, mult float64) {
	if math.IsNaN(mult) || math.IsInf(mult, 0) || mult <= 0 || mult == 1 {
		return // 0 = unset (partial user rows); never zero out a cost
	}
	// Scale in the float domain and clamp each component to maxComponent
	// (mulDiv's saturation ceiling) BEFORE converting back to int64. A raw
	// int64(float64(v)*mult) is implementation-defined once the product
	// leaves int64 range — on amd64 it yields MinInt64, a large NEGATIVE
	// cost, exactly the corrupted-ledger outcome mulDiv guards against. The
	// admin API allows multipliers up to 1000, so a saturated (or merely
	// large) component times a big multiplier is reachable with the same
	// hostile token counts mulDiv was written for. NaN is rejected above:
	// `mult <= 0 || mult == 1` is false for NaN, so it would otherwise slip
	// through and int64(NaN) is garbage. Four components each clamped to
	// maxComponent still sum into Total without overflow (maxComponent is
	// MaxInt64/4 by construction), so Total needs no clamp.
	scale := func(v *int64) {
		f := float64(*v) * mult
		switch {
		case !(f > 0): // NaN or underflow to 0
			*v = 0
		case f >= maxComponent:
			*v = maxComponent
		default:
			*v = int64(f + 0.5)
		}
	}
	scale(&c.In)
	scale(&c.Out)
	scale(&c.Cache)
	scale(&c.Write)
	c.Total = c.In + c.Out + c.Cache + c.Write
}
