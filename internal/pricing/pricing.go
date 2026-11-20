// Package pricing computes request cost snapshots from per-model prices.
// All money is integer micro-units; prices are micro-units per 1M tokens.
package pricing

import "llm-relay/internal/store"

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

// mulDiv computes tokens*price/million with round-half-up, all in int64.
func mulDiv(tokens, price int64) int64 {
	if tokens <= 0 || price <= 0 {
		return 0
	}
	return (tokens*price + million/2) / million
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
	if mult <= 0 || mult == 1 {
		return // 0 = unset (partial user rows); never zero out a cost
	}
	scale := func(v *int64) { *v = int64(float64(*v)*mult + 0.5) }
	scale(&c.In)
	scale(&c.Out)
	scale(&c.Cache)
	scale(&c.Write)
	c.Total = c.In + c.Out + c.Cache + c.Write
}
