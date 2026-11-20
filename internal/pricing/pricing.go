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
	billableIn := tok.Prompt - tok.Cached - tok.CacheWrite
	if billableIn < 0 {
		billableIn = 0
	}
	c := Costs{
		In:    mulDiv(billableIn, m.PriceIn),
		Out:   mulDiv(tok.Completion, m.PriceOut),
		Cache: mulDiv(tok.Cached, m.PriceCacheRead),
		Write: mulDiv(tok.CacheWrite, m.PriceCacheWrite),
	}
	c.Total = c.In + c.Out + c.Cache + c.Write
	return c
}
